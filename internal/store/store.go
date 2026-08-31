// Package store persists observations.
//
// Two design rules govern everything here, and both come from a mistake made in
// week 0: the falsification probe wrote only its own classification of the
// registry to disk and threw the registry's raw entries away. Every new question
// asked of that snapshot since then has been unanswerable. So:
//
//  1. Raw bytes are the record. Anything computed from them -- counts,
//     classifications, hashes -- is a derived view that can be rebuilt. If the
//     canonicalization rules change in month three, the whole history can be
//     re-derived instead of being lost.
//
//  2. Content addressing, not timestamps. A blob is stored under the SHA-256 of
//     its bytes, so a server whose tool surface does not change costs nothing to
//     observe again. The archive grows only when something actually changed,
//     which is precisely what a transparency log is for.
//
// There is deliberately no database yet. An append-only, single-writer,
// read-mostly workload with a few million rows a year is the one case where
// files plus a JSONL index genuinely beat Postgres, and it keeps the crawler at
// zero dependencies -- `go build` with nothing to download.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yhouta/mcp-observatory/internal/canon"
)

// SchemaVersion is stamped on every record. Bump it when the meaning of a field
// changes, never reuse a field for a new meaning: old records must stay readable.
const SchemaVersion = 1

// Record is one line of a run index: what we asked, what happened, and the
// content addresses of the bytes that prove it.
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	ObservedAt    string `json:"observed_at"` // RFC3339, UTC

	// Identity as the registry declared it.
	ServerName    string `json:"server_name"`
	ServerVersion string `json:"server_version,omitempty"`
	Endpoint      string `json:"endpoint"`
	Host          string `json:"host"`
	DeclaredType  string `json:"declared_type,omitempty"`

	// What actually happened on the wire.
	Outcome         string `json:"outcome"`
	Transport       string `json:"transport,omitempty"`
	Handshake       string `json:"handshake,omitempty"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	FailReason      string `json:"fail_reason,omitempty"`
	DurationMS      int64  `json:"duration_ms"`

	// The observation itself.
	ToolCount     int      `json:"tool_count"`
	ToolNames     []string `json:"tool_names,omitempty"`
	PageCount     int      `json:"page_count,omitempty"`
	Truncated     bool     `json:"truncated,omitempty"`
	SurfaceSHA256 string   `json:"surface_sha256,omitempty"` // canonical: the change key
	SurfaceError  string   `json:"surface_error,omitempty"`  // set if the response would not canonicalize

	// Content addresses of the archived raw bytes.
	InitBlob  string   `json:"init_blob,omitempty"`
	PageBlobs []string `json:"page_blobs,omitempty"`

	// Annotation census, derived. Kept on the line so the common queries need
	// no blob reads; always recomputable from the blobs if the rule changes.
	Annotated      int `json:"annotated,omitempty"`
	ReadOnlyHint   int `json:"read_only_hint,omitempty"`
	DestructiveHnt int `json:"destructive_hint,omitempty"`
	IdempotentHint int `json:"idempotent_hint,omitempty"`
	OpenWorldHint  int `json:"open_world_hint,omitempty"`
}

// Store writes blobs and one run index. It is safe for concurrent use.
type Store struct {
	root string

	mu    sync.Mutex
	index *os.File
	seen  map[string]bool // blobs written this run, to skip redundant stat calls
}

// Open prepares the store rooted at dir and starts a run index for runID.
func Open(dir, runID string) (*Store, error) {
	runDir := filepath.Join(dir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(runDir, "index.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Store{root: dir, index: f, seen: map[string]bool{}}, nil
}

func (s *Store) Close() error { return s.index.Close() }

// PutBlob stores b under its content address and returns that address.
// Writing the same bytes twice is a no-op, which is what makes a daily crawl of
// a mostly-unchanging ecosystem cheap.
func (s *Store) PutBlob(b []byte) (string, error) {
	if len(b) == 0 {
		return "", nil
	}
	sum := canon.SHA256Hex(b)

	s.mu.Lock()
	already := s.seen[sum]
	s.mu.Unlock()
	if already {
		return sum, nil
	}

	dir := filepath.Join(s.root, "blobs", sum[:2])
	path := filepath.Join(dir, sum)
	if _, err := os.Stat(path); err == nil {
		s.mu.Lock()
		s.seen[sum] = true
		s.mu.Unlock()
		return sum, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Write to a temp name then rename, so a crash never leaves a truncated blob
	// sitting at an address that claims to hash to something else.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		// A concurrent writer winning the race is fine: same bytes, same address.
		if _, serr := os.Stat(path); serr != nil {
			return "", err
		}
	}
	s.mu.Lock()
	s.seen[sum] = true
	s.mu.Unlock()
	return sum, nil
}

// Append writes one record as a line of the run index.
func (s *Store) Append(r *Record) error {
	r.SchemaVersion = SchemaVersion
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.index.Write(append(line, '\n'))
	return err
}

// RunMeta is the header written when a run finishes: enough to reproduce it.
type RunMeta struct {
	SchemaVersion int      `json:"schema_version"`
	RunID         string   `json:"run_id"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	CrawlerVer    string   `json:"crawler_version"`
	UserAgent     string   `json:"user_agent"`
	RegistryURL   string   `json:"registry_url"`
	RegistryBlobs []string `json:"registry_blobs"` // raw registry pages, in order

	Targets  int            `json:"targets"`
	Outcomes map[string]int `json:"outcomes"`
	Tools    int            `json:"tools_observed"`
	Notes    string         `json:"notes,omitempty"`

	// Complete says whether every intended endpoint was actually probed. An
	// interrupted run still writes its meta -- it must, or the observations
	// already taken would have no header -- so the absence of meta.json is not a
	// usable signal for "unfinished". Only this flag is.
	Complete bool `json:"complete"`
}

func (s *Store) WriteMeta(m *RunMeta) error {
	m.SchemaVersion = SchemaVersion
	m.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.root, "runs", m.RunID, "meta.json"), b, 0o644)
}

// NewRunID is a sortable, human-readable run identifier.
func NewRunID(t time.Time) string {
	return t.UTC().Format("2006-01-02T150405Z")
}

// ResumeMaxAge bounds how stale a run may be and still be resumable. An
// observation carries the date it was taken; appending today's observations to
// a run from last week would file them under the wrong day and quietly corrupt
// the log.
const ResumeMaxAge = 24 * time.Hour

// FindIncompleteRun returns the newest run that has not finished, or "" if the
// newest run is complete or too old to resume.
//
// It inspects ONLY the newest run and never scans further back. An earlier
// version walked backwards looking for a run with no meta.json, and on the
// first real interruption it skipped past today's run -- which had correctly
// written a meta saying "interrupted" -- and resumed a dead run from the
// previous day, appending today's observations under yesterday's date. Resuming
// anything other than the latest run is never the right answer.
func FindIncompleteRun(dir string) (string, error) {
	ents, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	newest := ""
	for _, e := range ents {
		if e.IsDir() && e.Name() > newest {
			newest = e.Name() // run IDs sort chronologically
		}
	}
	if newest == "" {
		return "", nil
	}

	runDir := filepath.Join(dir, "runs", newest)
	if _, err := os.Stat(filepath.Join(runDir, "index.jsonl")); err != nil {
		return "", nil // nothing was observed; a fresh run is cleaner
	}

	// A missing meta means the process died before writing one: resumable.
	// A present meta is authoritative about whether the run finished.
	b, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err == nil {
		var m RunMeta
		if err := json.Unmarshal(b, &m); err != nil {
			return "", fmt.Errorf("run %s has an unreadable meta.json: %w", newest, err)
		}
		if m.Complete {
			return "", nil
		}
	}

	started, err := time.Parse("2006-01-02T150405Z", newest)
	if err == nil && time.Since(started) > ResumeMaxAge {
		return "", fmt.Errorf("newest run %s is older than %s; start a fresh run instead",
			newest, ResumeMaxAge)
	}
	return newest, nil
}

// ObservedEndpoints returns the endpoints already recorded in a run's index,
// so a resumed run can skip them.
//
// A truncated final line (the process died mid-write) is skipped rather than
// treated as an error: the endpoint it named will simply be observed again,
// which costs one request and is far better than refusing to resume at all.
func ObservedEndpoints(dir, runID string) (map[string]bool, error) {
	f, err := os.Open(filepath.Join(dir, "runs", runID, "index.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20) // tool_names can make a line large
	for sc.Scan() {
		var r struct {
			ServerName string `json:"server_name"`
			Endpoint   string `json:"endpoint"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if r.Endpoint != "" {
			out[r.ServerName+"\x00"+r.Endpoint] = true
		}
	}
	// A scanner error on the last line is the truncated-write case; keep what we read.
	return out, nil
}

func (s *Store) Root() string { return s.root }

// Summary is a run's totals, counted from the index file rather than from
// in-memory counters. A resumed run's counters only cover the endpoints probed
// since the resume, so anything reported from memory would undercount the day.
// Deriving from the record is also the rule the rest of this project follows.
type Summary struct {
	Records  int
	Outcomes map[string]int
	Tools    int
}

func SummarizeRun(dir, runID string) (*Summary, error) {
	f, err := os.Open(filepath.Join(dir, "runs", runID, "index.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sum := &Summary{Outcomes: map[string]int{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		var r struct {
			Outcome   string `json:"outcome"`
			ToolCount int    `json:"tool_count"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		sum.Records++
		sum.Outcomes[r.Outcome]++
		sum.Tools += r.ToolCount
	}
	return sum, nil
}

var _ = fmt.Sprintf
