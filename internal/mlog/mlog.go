// Package mlog is the append-only, tamper-evident log the observations go into.
//
// It is a thin layer over golang.org/x/mod/sumdb/tlog, which is the same
// RFC 6962 implementation that backs Go's own checksum database. That choice is
// deliberate and worth stating in any description of this project: hand-rolling
// a Merkle tree is not hard to get working and is very easy to get subtly
// wrong, and a log whose proofs only verify against its author's own code is
// not meaningfully verifiable at all.
//
// What the structure buys, concretely: once an observation is in the tree, it
// cannot be edited, reordered or removed without changing the root hash. Since
// the root is signed and published every day, and other people keep copies of
// yesterday's roots, there is no version of history the operator can quietly
// substitute later. The operator does not have to be trusted -- which is the
// entire point, because the operator is you.
package mlog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/mod/sumdb/tlog"
)

// hashSize is the width of one stored tlog hash.
const hashSize = tlog.HashSize

// Log is an append-only tlog backed by two files:
//
//	hashes   every stored tree hash, hashSize bytes each, in tlog index order
//	size     the number of leaves, so the tree can be reopened
//
// Leaves themselves are not duplicated here: they live in the run indexes,
// which are the actual record. This file holds only what is needed to prove
// things about them.
type Log struct {
	dir    string
	hashes *os.File
	n      int64 // leaf count
}

func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "hashes"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Log{dir: dir, hashes: f}

	b, err := os.ReadFile(filepath.Join(dir, "size"))
	switch {
	case err == nil:
		if len(b) < 8 {
			f.Close()
			return nil, errors.New("mlog: size file is truncated")
		}
		l.n = int64(binary.BigEndian.Uint64(b[:8]))
	case os.IsNotExist(err):
		l.n = 0
	default:
		f.Close()
		return nil, err
	}

	// The stored-hash file must contain exactly the hashes implied by the leaf
	// count. A mismatch means a crash mid-append or an edited file, and
	// continuing would silently build proofs over a tree nobody can reproduce.
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if want := tlog.StoredHashCount(l.n) * hashSize; fi.Size() != want {
		f.Close()
		return nil, fmt.Errorf("mlog: hashes file is %d bytes, expected %d for %d leaves",
			fi.Size(), want, l.n)
	}
	return l, nil
}

func (l *Log) Close() error { return l.hashes.Close() }

// Size is the number of leaves currently in the tree.
func (l *Log) Size() int64 { return l.n }

// ReadHashes implements tlog.HashReader over the stored-hash file.
func (l *Log) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	out := make([]tlog.Hash, len(indexes))
	for i, idx := range indexes {
		var h tlog.Hash
		if _, err := l.hashes.ReadAt(h[:], idx*hashSize); err != nil {
			return nil, fmt.Errorf("mlog: read hash %d: %w", idx, err)
		}
		out[i] = h
	}
	return out, nil
}

// Append adds records to the tree and returns the new tree size.
//
// Appending is the only mutation this package offers. There is deliberately no
// way to replace or delete a leaf: an append-only interface is what makes the
// "cannot be rewritten" claim a property of the code rather than a promise.
func (l *Log) Append(records [][]byte) (int64, error) {
	if len(records) == 0 {
		return l.n, nil
	}

	// tlog appends one record at a time, and each record's hashes may depend on
	// hashes produced by the records just before it. Those are not on disk yet,
	// so reads during the batch have to fall through to a pending buffer.
	base := tlog.StoredHashCount(l.n)
	pend := &pending{log: l, base: base}
	n := l.n
	for _, rec := range records {
		hs, err := tlog.StoredHashes(n, rec, pend)
		if err != nil {
			return 0, err
		}
		pend.hashes = append(pend.hashes, hs...)
		n++
	}

	// Write the hashes first, then the size. If the process dies between the
	// two, Open's length check rejects the log instead of trusting a tree whose
	// declared size does not match its contents.
	buf := make([]byte, 0, len(pend.hashes)*hashSize)
	for _, h := range pend.hashes {
		buf = append(buf, h[:]...)
	}
	at := tlog.StoredHashCount(l.n) * hashSize
	if _, err := l.hashes.WriteAt(buf, at); err != nil {
		return 0, err
	}
	if err := l.hashes.Sync(); err != nil {
		return 0, err
	}

	if err := l.writeSize(n); err != nil {
		return 0, err
	}
	l.n = n
	return n, nil
}

// pending serves hashes from the file, falling back to those computed earlier
// in the current batch and not yet written.
type pending struct {
	log    *Log
	base   int64 // stored-hash index of the first pending hash
	hashes []tlog.Hash
}

func (p *pending) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	out := make([]tlog.Hash, len(indexes))
	var fromFile []int64
	var slots []int
	for i, idx := range indexes {
		if idx >= p.base {
			k := idx - p.base
			if k >= int64(len(p.hashes)) {
				return nil, fmt.Errorf("mlog: hash %d not yet computed", idx)
			}
			out[i] = p.hashes[k]
			continue
		}
		fromFile = append(fromFile, idx)
		slots = append(slots, i)
	}
	if len(fromFile) > 0 {
		hs, err := p.log.ReadHashes(fromFile)
		if err != nil {
			return nil, err
		}
		for j, slot := range slots {
			out[slot] = hs[j]
		}
	}
	return out, nil
}

func (l *Log) writeSize(n int64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	tmp := filepath.Join(l.dir, "size.tmp")
	if err := os.WriteFile(tmp, b[:], 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.dir, "size"))
}

// TreeHash is the root hash covering every leaf appended so far. This is the
// value that gets signed and published.
func (l *Log) TreeHash() (tlog.Hash, error) {
	if l.n == 0 {
		return tlog.Hash{}, errors.New("mlog: tree is empty")
	}
	return tlog.TreeHash(l.n, l)
}

// ProveRecord builds an inclusion proof for leaf i against the current tree.
// The proof is about twenty hashes regardless of how many million leaves the
// tree holds, which is what makes verifying one observation cheap.
func (l *Log) ProveRecord(i int64) (tlog.RecordProof, error) {
	if i < 0 || i >= l.n {
		return nil, fmt.Errorf("mlog: leaf %d out of range (size %d)", i, l.n)
	}
	return tlog.ProveRecord(l.n, i, l)
}

// ProveTree builds a consistency proof showing the tree of size oldN is a
// prefix of the current tree -- that history was appended to, not rewritten.
// This is the proof that actually catches a dishonest operator, so it matters
// more than inclusion: it is what a verifier runs against the head it saved
// yesterday.
func (l *Log) ProveTree(oldN int64) (tlog.TreeProof, error) {
	if oldN <= 0 || oldN > l.n {
		return nil, fmt.Errorf("mlog: old size %d out of range (size %d)", oldN, l.n)
	}
	return tlog.ProveTree(l.n, oldN, l)
}

// RecordHash is the leaf hash of a record, exported so callers can check a
// record against a proof without importing tlog themselves.
func RecordHash(data []byte) tlog.Hash { return tlog.RecordHash(data) }

var _ tlog.HashReader = (*Log)(nil)
var _ = io.Discard
