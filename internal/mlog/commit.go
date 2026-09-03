package mlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// A leaf is one line of a run index, exactly as it was written.
//
// Choosing the raw line as the leaf makes the tree reproducible by anyone
// holding the indexes: they concatenate runs in chronological order, take the
// lines in file order, and must arrive at the same root. Anything cleverer --
// re-serialising, sorting, normalising -- would mean a verifier has to
// reimplement my formatting decisions exactly before they could disagree with
// me, which defeats the purpose.
//
// The consequence is that the index files are themselves part of the record and
// must never be rewritten. That is already true of an append-only log; this
// just makes it load-bearing.

// manifestEntry records which run contributed which range of leaves.
type manifestEntry struct {
	RunID     string `json:"run_id"`
	FirstLeaf int64  `json:"first_leaf"`
	Leaves    int64  `json:"leaves"`
}

func manifestPath(logDir string) string { return filepath.Join(logDir, "manifest.jsonl") }

// ReadManifest returns the runs committed to the tree, in commit order.
func ReadManifest(logDir string) ([]manifestEntry, error) {
	f, err := os.Open(manifestPath(logDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []manifestEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e manifestEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// CommitRun appends any lines of a run index that are not yet in the tree and
// returns how many were added.
//
// It is written to be safe to call repeatedly, because a resumed census calls
// it more than once for the same run: only the lines beyond what the manifest
// already accounts for get appended. Committing the same observation twice
// would not corrupt the tree, but it would make the leaf count disagree with
// the number of observations, and every published figure derived from the log
// would then be quietly wrong.
func CommitRun(logDir, dataDir, runID string) (int64, error) {
	l, err := Open(logDir)
	if err != nil {
		return 0, err
	}
	defer l.Close()

	man, err := ReadManifest(logDir)
	if err != nil {
		return 0, err
	}
	var already int64
	for _, e := range man {
		if e.RunID == runID {
			already += e.Leaves
		}
	}

	lines, err := readIndexLines(dataDir, runID)
	if err != nil {
		return 0, err
	}
	if int64(len(lines)) <= already {
		return 0, nil // nothing new
	}

	first := l.Size()
	add := lines[already:]
	if _, err := l.Append(add); err != nil {
		return 0, err
	}
	e := manifestEntry{RunID: runID, FirstLeaf: first, Leaves: int64(len(add))}
	if err := appendManifest(logDir, e); err != nil {
		return 0, err
	}
	return int64(len(add)), nil
}

func appendManifest(logDir string, e manifestEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(manifestPath(logDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// readIndexLines returns a run index as leaf records, one per line.
// A trailing partial line is dropped: the crawler writes whole lines, so an
// incomplete one means the process died mid-write and the observation it
// describes was never finished.
func readIndexLines(dataDir, runID string) ([][]byte, error) {
	f, err := os.Open(filepath.Join(dataDir, "runs", runID, "index.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			continue
		}
		out = append(out, append([]byte(nil), line...))
	}
	return out, sc.Err()
}

// LeafRange reports where a run's leaves sit in the tree, for building proofs
// about a particular day.
func LeafRange(logDir, runID string) (first, count int64, err error) {
	man, err := ReadManifest(logDir)
	if err != nil {
		return 0, 0, err
	}
	found := false
	for _, e := range man {
		if e.RunID != runID {
			continue
		}
		if !found {
			first, found = e.FirstLeaf, true
		}
		count += e.Leaves
	}
	if !found {
		return 0, 0, fmt.Errorf("run %s is not in the tree", runID)
	}
	return first, count, nil
}

// Recompute rebuilds the tree from the run indexes named in the manifest and
// returns the resulting size and root. This is what verification actually
// runs: it never trusts the stored hash file, only the records.
func Recompute(logDir, dataDir string) (int64, string, error) {
	man, err := ReadManifest(logDir)
	if err != nil {
		return 0, "", err
	}
	tmp, err := os.MkdirTemp("", "mlog-verify-*")
	if err != nil {
		return 0, "", err
	}
	defer os.RemoveAll(tmp)

	l, err := Open(tmp)
	if err != nil {
		return 0, "", err
	}
	defer l.Close()

	seen := map[string]int64{}
	for _, e := range man {
		lines, err := readIndexLines(dataDir, e.RunID)
		if err != nil {
			return 0, "", fmt.Errorf("run %s: %w", e.RunID, err)
		}
		start := seen[e.RunID]
		end := start + e.Leaves
		if int64(len(lines)) < end {
			return 0, "", fmt.Errorf("run %s: manifest claims %d leaves, index has %d",
				e.RunID, end, len(lines))
		}
		if _, err := l.Append(lines[start:end]); err != nil {
			return 0, "", err
		}
		seen[e.RunID] = end
	}
	if l.Size() == 0 {
		return 0, "", nil
	}
	root, err := l.TreeHash()
	if err != nil {
		return 0, "", err
	}
	return l.Size(), root.String(), nil
}
