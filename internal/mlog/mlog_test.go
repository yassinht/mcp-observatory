package mlog

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/mod/sumdb/tlog"
)

func records(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf(`{"server":"s%d","surface":"hash-%d"}`, i, i))
	}
	return out
}

func openTemp(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "tlog"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestAppendAndInclusionProof(t *testing.T) {
	l := openTemp(t)
	recs := records(1000)
	if _, err := l.Append(recs); err != nil {
		t.Fatal(err)
	}
	if l.Size() != 1000 {
		t.Fatalf("size = %d, want 1000", l.Size())
	}
	root, err := l.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int64{0, 1, 499, 998, 999} {
		p, err := l.ProveRecord(i)
		if err != nil {
			t.Fatalf("prove %d: %v", i, err)
		}
		if err := tlog.CheckRecord(p, l.Size(), root, i, tlog.RecordHash(recs[i])); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
}

// The claim this project makes is that an observation cannot be altered after
// the fact without detection. This is that claim, as a test.
func TestTamperedRecordIsRejected(t *testing.T) {
	l := openTemp(t)
	recs := records(500)
	if _, err := l.Append(recs); err != nil {
		t.Fatal(err)
	}
	root, err := l.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.ProveRecord(42)
	if err != nil {
		t.Fatal(err)
	}

	// The rug-pull edit: same server, one character of the recorded surface
	// changed after it was logged.
	forged := []byte(`{"server":"s42","surface":"hash-99"}`)
	if err := tlog.CheckRecord(p, l.Size(), root, 42, tlog.RecordHash(forged)); err == nil {
		t.Fatal("a forged record verified against the published root")
	}
}

// Appending must extend history, never replace it. A verifier who saved
// yesterday's head checks exactly this.
func TestConsistencyProofAcceptsHonestAppend(t *testing.T) {
	l := openTemp(t)
	if _, err := l.Append(records(300)); err != nil {
		t.Fatal(err)
	}
	oldRoot, err := l.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	oldSize := l.Size()

	more := make([][]byte, 0, 120)
	for i := 300; i < 420; i++ {
		more = append(more, []byte(fmt.Sprintf(`{"server":"s%d","surface":"hash-%d"}`, i, i)))
	}
	if _, err := l.Append(more); err != nil {
		t.Fatal(err)
	}
	newRoot, err := l.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.ProveTree(oldSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := tlog.CheckTree(p, l.Size(), newRoot, oldSize, oldRoot); err != nil {
		t.Fatalf("honest append failed its consistency proof: %v", err)
	}
}

// The dishonest case: an operator rebuilds the log with one early entry
// changed, then publishes the new root as if it were a continuation. Anyone
// holding the older head must be able to tell.
func TestRewrittenHistoryFailsConsistency(t *testing.T) {
	honest := openTemp(t)
	recs := records(300)
	if _, err := honest.Append(recs); err != nil {
		t.Fatal(err)
	}
	oldRoot, err := honest.TreeHash()
	if err != nil {
		t.Fatal(err)
	}

	// Rebuild from scratch with leaf 7 quietly altered, then grow past the
	// original size so the sizes alone look plausible.
	rewritten := openTemp(t)
	altered := records(300)
	altered[7] = []byte(`{"server":"s7","surface":"ATTACKER"}`)
	if _, err := rewritten.Append(altered); err != nil {
		t.Fatal(err)
	}
	if _, err := rewritten.Append(records(50)); err != nil {
		t.Fatal(err)
	}
	newRoot, err := rewritten.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	p, err := rewritten.ProveTree(300)
	if err != nil {
		t.Fatal(err)
	}
	if err := tlog.CheckTree(p, rewritten.Size(), newRoot, 300, oldRoot); err == nil {
		t.Fatal("a rewritten log passed a consistency check against the original head")
	}
}

func TestReopenPreservesTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tlog")
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(records(257)); err != nil { // spans a power-of-two boundary
		t.Fatal(err)
	}
	want, err := l.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.Size() != 257 {
		t.Fatalf("reopened size = %d, want 257", l2.Size())
	}
	got, err := l2.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root changed across reopen:\n %v\n %v", got, want)
	}
}

func TestHeadRoundTripAndSignature(t *testing.T) {
	l := openTemp(t)
	if _, err := l.Append(records(64)); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h, err := l.Sign(priv, time.Date(2026, 9, 2, 3, 41, 7, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseHead([]byte(h.String()))
	if err != nil {
		t.Fatalf("parse own head: %v", err)
	}
	if parsed.Size != h.Size || parsed.Root != h.Root || !parsed.Time.Equal(h.Time) {
		t.Fatalf("round trip changed the head:\n %+v\n %+v", parsed, h)
	}
	if err := parsed.Verify(pub); err != nil {
		t.Fatalf("valid head rejected: %v", err)
	}

	// A head whose size is edited must stop verifying, or publishing the head
	// proves nothing about the tree it claims to describe.
	parsed.Size++
	if err := parsed.Verify(pub); err == nil {
		t.Fatal("an edited head still verified")
	}

	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := h.Verify(other); err == nil {
		t.Fatal("head verified under the wrong public key")
	}
}

func TestParseHeadRejectsMalformed(t *testing.T) {
	for name, in := range map[string]string{
		"empty":        "",
		"wrong format": "some-other-log/v1\nsize 1\nroot AAAA\ntime x\nsig  AA\n",
		"short":        headFormat + "\nsize 1\n",
		"bad size":     headFormat + "\nsize abc\nroot AAAA\ntime 2026-09-02T00:00:00Z\nsig  AA\n",
	} {
		if _, err := ParseHead([]byte(in)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}
