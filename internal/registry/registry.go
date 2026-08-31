// Package registry pulls the official MCP registry and hands back both the
// parsed targets and the raw page bytes.
//
// The raw pages are archived on every run. The registry is itself a moving
// target -- entries appear, get deprecated, change their declared endpoint --
// and an observatory that keeps only its own reading of the registry cannot
// later answer "what did the registry say that day?". That question turns out
// to matter: a server whose endpoint silently changed in the registry looks
// identical to a server that moved, unless both readings were kept.
//
// Fetching all 248 pages takes about an hour on a slow link, so two properties
// are not optional: every page is retried before the walk is abandoned, and
// every page is written to a resumable cache as it arrives. The first version
// of this file had neither, and a single transient timeout at page 36 discarded
// thirty-five pages of completed work.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const DefaultURL = "https://registry.modelcontextprotocol.io/v0/servers"

// Entry is the slice of a registry record this crawler acts on. The full record
// stays in the archived page bytes.
type Entry struct {
	Name     string
	Version  string
	Title    string
	Status   string
	Endpoint string
	Type     string // declared transport: streamable-http | sse
	Host     string
}

type rawEntry struct {
	Server struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Title   string `json:"title"`
		Remotes []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"remotes"`
	} `json:"server"`
	Meta struct {
		Official struct {
			Status string `json:"status"`
		} `json:"io.modelcontextprotocol.registry/official"`
	} `json:"_meta"`
}

type page struct {
	Servers  []rawEntry `json:"servers"`
	Metadata struct {
		// The registry serves camelCase. snake_case is accepted too: reading only
		// one spelling silently truncates the crawl at page 1 with no error at
		// all, which is exactly what happened on the first run of this crawler.
		NextCursor      string `json:"nextCursor"`
		NextCursorSnake string `json:"next_cursor"`
		Count           int    `json:"count"`
	} `json:"metadata"`
}

func (p *page) cursor() string {
	if p.Metadata.NextCursor != "" {
		return p.Metadata.NextCursor
	}
	return p.Metadata.NextCursorSnake
}

type Client struct {
	URL       string
	UserAgent string
	HTTP      *http.Client
	MaxPages  int

	// CacheDir, when set, stores each page as it arrives and lets a later run
	// resume instead of starting over.
	CacheDir string
	// Refresh discards any cached pages and walks the registry from the start.
	Refresh bool
	// Retries is the number of extra attempts per page before giving up.
	Retries int
}

func New(ua string) *Client {
	return &Client{
		URL:       DefaultURL,
		UserAgent: ua,
		// Generous: the registry is occasionally slow, and the alternative to
		// waiting is discarding the whole walk.
		HTTP:     &http.Client{Timeout: 90 * time.Second},
		MaxPages: 2000,
		Retries:  4,
	}
}

// Result reports how a walk went, so the caller can tell a complete registry
// reading from a partial one. Publishing a census computed from a truncated
// walk is the failure mode this field exists to prevent.
type Result struct {
	Targets   []Entry
	Pages     [][]byte
	Entries   int
	FromCache int
	Fetched   int
	Complete  bool   // the walk reached a page with no next cursor
	StoppedAt string // why it stopped, if not complete
}

// FetchAll walks the whole registry, returning one Entry per declared remote
// endpoint -- a server listing two remotes is two things to observe, not one.
func (c *Client) FetchAll(ctx context.Context, progress func(entries, pages int)) (*Result, error) {
	res := &Result{}

	var pages [][]byte
	cursor := ""

	// ------------------------------------------------------------- cache
	if c.CacheDir != "" && !c.Refresh {
		cached, err := loadCache(c.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("registry cache: %w", err)
		}
		pages = cached
		res.FromCache = len(cached)
		if n := len(cached); n > 0 {
			var last page
			if err := json.Unmarshal(cached[n-1], &last); err != nil {
				return nil, fmt.Errorf("registry cache: last page unreadable: %w", err)
			}
			cursor = last.cursor()
			if cursor == "" {
				res.Complete = true // the cached walk already reached the end
			}
		}
	}

	// ------------------------------------------------------------- fetch
	for len(pages) < c.MaxPages && !res.Complete {
		if err := ctx.Err(); err != nil {
			res.StoppedAt = "interrupted"
			break
		}
		body, err := c.fetchPage(ctx, cursor)
		if err != nil {
			// Everything fetched so far is already cached and returned; the caller
			// decides whether a partial reading is usable.
			res.StoppedAt = fmt.Sprintf("page %d: %v", len(pages), err)
			break
		}
		var p page
		if err := json.Unmarshal(body, &p); err != nil {
			res.StoppedAt = fmt.Sprintf("page %d: malformed json: %v", len(pages), err)
			break
		}
		if c.CacheDir != "" {
			if err := writeCachePage(c.CacheDir, len(pages), body); err != nil {
				return nil, fmt.Errorf("registry cache write: %w", err)
			}
		}
		pages = append(pages, body)
		res.Fetched++

		if progress != nil {
			progress(len(pages)*100, len(pages))
		}

		next := p.cursor()
		if next == "" || len(p.Servers) == 0 {
			res.Complete = true
			break
		}
		if next == cursor {
			res.StoppedAt = "registry returned a repeating cursor"
			break
		}
		cursor = next
		time.Sleep(100 * time.Millisecond) // be a polite guest
	}

	// ------------------------------------------------------------- parse
	seen := map[string]bool{}
	for i, body := range pages {
		var p page
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("registry page %d: %w", i, err)
		}
		for _, e := range p.Servers {
			res.Entries++
			for _, rem := range e.Server.Remotes {
				host, ok := hostOf(rem.URL)
				if !ok {
					// Unparseable or templated URLs (the registry contains entries
					// with a literal "{host}" placeholder). Skipped for probing but
					// still present in the archived page bytes.
					continue
				}
				key := e.Server.Name + "\x00" + rem.URL
				if seen[key] {
					continue
				}
				seen[key] = true
				res.Targets = append(res.Targets, Entry{
					Name:     e.Server.Name,
					Version:  e.Server.Version,
					Title:    e.Server.Title,
					Status:   e.Meta.Official.Status,
					Endpoint: rem.URL,
					Type:     rem.Type,
					Host:     host,
				})
			}
		}
	}
	res.Pages = pages
	return res, nil
}

// fetchPage gets one page, retrying transient failures with backoff. A timeout
// on page 36 of 248 must not discard pages 1 through 35.
func (c *Client) fetchPage(ctx context.Context, cursor string) ([]byte, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("limit", "100")
	q.Set("version", "latest")
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	u.RawQuery = q.Encode()

	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<uint(attempt-1)) * 2 * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", "application/json")

		resp, derr := c.HTTP.Do(req)
		if derr != nil {
			lastErr = derr
			continue
		}
		body, berr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if berr != nil {
			lastErr = berr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("http %d", resp.StatusCode)
			// 4xx other than 429 will not improve with retrying.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return nil, lastErr
			}
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("after %d attempts: %w", c.Retries+1, lastErr)
}

// ---------------------------------------------------------------- cache

func cachePath(dir string, n int) string {
	return filepath.Join(dir, fmt.Sprintf("page-%04d.json", n))
}

func writeCachePage(dir string, n int, body []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := cachePath(dir, n) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cachePath(dir, n))
}

// loadCache reads cached pages in order, stopping at the first gap: pages 0..k
// are usable, and anything after a missing page cannot be trusted to follow on.
func loadCache(dir string) ([][]byte, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "page-") && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out [][]byte
	for i, name := range names {
		if name != fmt.Sprintf("page-%04d.json", i) {
			break // gap: stop here rather than splicing unrelated pages together
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ClearCache removes a cached registry walk.
func ClearCache(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// hostOf rejects anything that is not a plain http(s) URL with a real host:
// templated placeholders, loopback addresses, and malformed strings.
func hostOf(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "{}") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case h == "localhost", strings.HasPrefix(h, "127."), h == "::1", h == "0.0.0.0":
		return "", false
	}
	return u.Host, true
}
