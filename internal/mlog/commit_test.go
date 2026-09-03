package mlog

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRun fakes what the crawler produces: a run directory with an index of
// one JSON observation per line.
func writeRun(t *testing.T, dataDir, runID string, n int) {
	t.Helper()
	dir := filepath.Join(dataDir, "runs", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"run_id":%q,"server_name":"s%d","endpoint":"https://e%d/mcp","outcome":"ok","surface_sha256":"h%d"}`+"\n",
			runID, i, i, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCommitAndRecompute(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	logDir := filepath.Join(base, "tlog")

	writeRun(t, dataDir, "2026-09-01T030000Z", 400)
	writeRun(t, dataDir, "2026-09-02T030000Z", 420)

	for _, run := range []string{"2026-09-01T030000Z", "2026-09-02T030000Z"} {
		added, err := CommitRun(logDir, dataDir, run)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: +%d leaves", run, added)
	}

	l, err := Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Size() != 820 {
		t.Fatalf("tree size = %d, want 820", l.Size())
	}
	stored, err := l.TreeHash()
	if err != nil {
		t.Fatal(err)
	}

	size, root, err := Recompute(logDir, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if size != 820 || root != stored.String() {
		t.Fatalf("recomputed tree disagrees with the stored one:\n %d %s\n %d %s",
			size, root, l.Size(), stored.String())
	}
}

// Committing the same run twice must be a no-op. A resumed census calls
// CommitRun again for a run already partly in the tree, and double-counting
// would make the leaf count disagree with the number of observations.
func TestCommitIsIdempotentAndResumable(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	logDir := filepath.Join(base, "tlog")
	run := "2026-09-01T030000Z"

	writeRun(t, dataDir, run, 100)
	if n, err := CommitRun(logDir, dataDir, run); err != nil || n != 100 {
		t.Fatalf("first commit: n=%d err=%v", n, err)
	}
	if n, err := CommitRun(logDir, dataDir, run); err != nil || n != 0 {
		t.Fatalf("second commit should add nothing: n=%d err=%v", n, err)
	}

	// The run is resumed and its index grows; only the new lines get appended.
	writeRun(t, dataDir, run, 175)
	if n, err := CommitRun(logDir, dataDir, run); err != nil || n != 75 {
		t.Fatalf("resumed commit: n=%d err=%v, want 75", n, err)
	}
	first, count, err := LeafRange(logDir, run)
	if err != nil {
		t.Fatal(err)
	}
	if first != 0 || count != 175 {
		t.Fatalf("leaf range = (%d,%d), want (0,175)", first, count)
	}
}

// The end-to-end claim: an observation edited after publication cannot survive
// a check against the signed head. This is the whole project in one test.
func TestEditedObservationBreaksTheSignedHead(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	logDir := filepath.Join(base, "tlog")
	run := "2026-09-01T030000Z"

	writeRun(t, dataDir, run, 250)
	if _, err := CommitRun(logDir, dataDir, run); err != nil {
		t.Fatal(err)
	}

	l, err := Open(logDir)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	head, err := l.Sign(priv, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	if err := head.Verify(pub); err != nil {
		t.Fatalf("freshly signed head does not verify: %v", err)
	}
	size, root, err := Recompute(logDir, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if size != head.Size || root != head.Root.String() {
		t.Fatal("records do not reproduce the head they were signed under")
	}

	// Now the operator quietly rewrites one archived observation: the tool
	// surface a server served on that day is changed after the fact.
	path := filepath.Join(dataDir, "runs", run, "index.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), `"surface_sha256":"h77"`, `"surface_sha256":"FORGED"`, 1)
	if edited == string(b) {
		t.Fatal("test setup: nothing was replaced")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	// The signature still verifies -- it always will, the operator holds the key.
	// What fails is the tree: the records no longer produce the root that was
	// signed. That is the property signing alone cannot give you.
	if err := head.Verify(pub); err != nil {
		t.Fatal("signature should still verify; the key did not change")
	}
	size2, root2, err := Recompute(logDir, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if size2 != head.Size {
		t.Fatalf("size changed unexpectedly: %d", size2)
	}
	if root2 == head.Root.String() {
		t.Fatal("an edited observation still reproduced the published root")
	}
}
