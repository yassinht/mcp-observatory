package mlog

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/sumdb/tlog"
)

// A signed tree head is the whole public surface of this log. Everything else
// -- gigabytes of archived responses -- is only meaningful because a few
// hundred signed bytes a day pin it in place.
//
// The format is plain text on purpose. Someone auditing this project should be
// able to read a head, and diff two of them, without running any of my code:
//
//	mcp-observatory/v1
//	size 4203891
//	root nDq3d1s+2y0mA1Lz...
//	time 2026-09-02T03:41:07Z
//	sig  MEUCIQDx...
//
// The signature covers the first four lines exactly as written, newline
// included, so what is verified is what a human reads.

const headFormat = "mcp-observatory/v1"

type Head struct {
	Size int64
	Root tlog.Hash
	Time time.Time
	Sig  []byte
}

// Body is the signed portion: everything except the signature line.
func (h *Head) Body() []byte {
	return []byte(fmt.Sprintf("%s\nsize %d\nroot %s\ntime %s\n",
		headFormat, h.Size, h.Root.String(), h.Time.UTC().Format(time.RFC3339)))
}

func (h *Head) String() string {
	return string(h.Body()) + "sig  " + base64.StdEncoding.EncodeToString(h.Sig) + "\n"
}

// Sign produces a signed head for the current tree.
func (l *Log) Sign(key ed25519.PrivateKey, now time.Time) (*Head, error) {
	root, err := l.TreeHash()
	if err != nil {
		return nil, err
	}
	h := &Head{Size: l.n, Root: root, Time: now.UTC()}
	h.Sig = ed25519.Sign(key, h.Body())
	return h, nil
}

// ParseHead reads a head back. It is deliberately strict: a head that does not
// parse exactly is refused rather than interpreted generously, because a
// verifier that guesses at malformed input is a verifier that can be fooled.
func ParseHead(b []byte) (*Head, error) {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 5 {
		return nil, fmt.Errorf("head: expected 5 lines, got %d", len(lines))
	}
	if lines[0] != headFormat {
		return nil, fmt.Errorf("head: unknown format %q", lines[0])
	}
	h := new(Head)

	var err error
	if h.Size, err = strconv.ParseInt(strings.TrimPrefix(lines[1], "size "), 10, 64); err != nil {
		return nil, fmt.Errorf("head: bad size line: %w", err)
	}
	if h.Root, err = tlog.ParseHash(strings.TrimPrefix(lines[2], "root ")); err != nil {
		return nil, fmt.Errorf("head: bad root line: %w", err)
	}
	if h.Time, err = time.Parse(time.RFC3339, strings.TrimPrefix(lines[3], "time ")); err != nil {
		return nil, fmt.Errorf("head: bad time line: %w", err)
	}
	if h.Sig, err = base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(lines[4], "sig "))); err != nil {
		return nil, fmt.Errorf("head: bad signature line: %w", err)
	}
	return h, nil
}

// Verify checks the signature against a public key.
func (h *Head) Verify(pub ed25519.PublicKey) error {
	if !ed25519.Verify(pub, h.Body(), h.Sig) {
		return errors.New("head: signature does not verify")
	}
	return nil
}

// ---------------------------------------------------------------- keys

// GenerateKey writes a new signing key, refusing to overwrite an existing one.
// Replacing the key of a running log silently invalidates every head already
// published, so this has to be an explicit act, never a side effect.
func GenerateKey(path string) (ed25519.PublicKey, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("key already exists at %s; refusing to overwrite", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(priv) + "\n"
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path+".pub", []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return nil, err
	}
	return pub, nil
}

func LoadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("key: expected %d bytes, got %d", ed25519.PrivateKeySize, len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("pubkey: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pubkey: expected %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
