// Command crawler observes every publicly reachable MCP server in the official
// registry and archives what each one served.
//
// One run produces:
//
//	data/blobs/<ab>/<sha256>        exact response bytes, deduplicated
//	data/runs/<runID>/index.jsonl   one line per endpoint observed
//	data/runs/<runID>/meta.json     run header, incl. the raw registry pages
//
// Politeness is structural rather than advisory: targets are grouped by host and
// each host is worked by exactly one goroutine with a delay between requests, so
// the two operators that between them publish a sixth of the registry cannot be
// hammered no matter how high --workers goes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yhouta/mcp-observatory/internal/canon"
	"github.com/yhouta/mcp-observatory/internal/probe"
	"github.com/yhouta/mcp-observatory/internal/registry"
	"github.com/yhouta/mcp-observatory/internal/store"
)

const version = "0.1.0"

var userAgent = "mcp-observatory/" + version +
	" (+https://github.com/yhouta/mcp-observatory; public transparency log; contact: yassine.houta@outlook.fr)"

func main() {
	var (
		dataDir  = flag.String("data", "data", "directory for blobs and run indexes")
		workers  = flag.Int("workers", 24, "hosts probed in parallel")
		delay    = flag.Duration("delay", 500*time.Millisecond, "pause between requests to the same host")
		timeout  = flag.Duration("timeout", 20*time.Second, "per-request timeout")
		limit    = flag.Int("limit", 0, "probe at most N targets (0 = all)")
		sample   = flag.Int("sample", 0, "probe a random sample of N targets (0 = all)")
		seed     = flag.Int64("seed", 42, "sample seed, for reproducibility")
		onlyHost = flag.String("host", "", "probe only this host")
		dryRun   = flag.Bool("dry-run", false, "fetch and archive the registry, probe nothing")

		cacheDir = flag.String("registry-cache", "data/registry-cache",
			"resumable cache of registry pages (empty string disables)")
		refresh = flag.Bool("refresh-registry", false,
			"discard the cached registry and walk it again from the start")
		allowPartial = flag.Bool("allow-partial", false,
			"proceed even if the registry walk did not reach the end")
		resume = flag.Bool("resume", false,
			"continue the most recent interrupted run instead of starting a new one")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now().UTC()
	runID := store.NewRunID(started)

	// Resuming keeps the original run ID: an observation belongs to the day it
	// was taken, and splitting one census across two IDs would make the log
	// claim two partial days instead of one complete one.
	var alreadyObserved map[string]bool
	if *resume {
		prev, err := store.FindIncompleteRun(*dataDir)
		if err != nil {
			fatal("find incomplete run: %v", err)
		}
		if prev == "" {
			fmt.Fprintln(os.Stderr, "no interrupted run found; starting a new one")
		} else {
			runID = prev
			alreadyObserved, err = store.ObservedEndpoints(*dataDir, runID)
			if err != nil {
				fatal("read previous index: %v", err)
			}
			fmt.Fprintf(os.Stderr, "resuming run %s (%d endpoints already observed)\n",
				runID, len(alreadyObserved))
		}
	}

	st, err := store.Open(*dataDir, runID)
	if err != nil {
		fatal("open store: %v", err)
	}
	defer st.Close()

	meta := &store.RunMeta{
		RunID:       runID,
		StartedAt:   started.Format(time.RFC3339),
		CrawlerVer:  version,
		UserAgent:   userAgent,
		RegistryURL: registry.DefaultURL,
		Outcomes:    map[string]int{},
	}

	// ---------------------------------------------------------- registry
	fmt.Fprintf(os.Stderr, "run %s\nfetching registry...\n", runID)
	rc := registry.New(userAgent)
	rc.CacheDir, rc.Refresh = *cacheDir, *refresh
	reg, err := rc.FetchAll(ctx, func(entries, n int) {
		fmt.Fprintf(os.Stderr, "\r  %d pages", n)
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fatal("registry: %v", err)
	}
	for _, p := range reg.Pages {
		h, perr := st.PutBlob(p)
		if perr != nil {
			fatal("archive registry page: %v", perr)
		}
		meta.RegistryBlobs = append(meta.RegistryBlobs, h)
	}
	targets := reg.Targets

	fmt.Fprintf(os.Stderr, "  %d pages (%d cached, %d fetched), %d entries, %d probeable endpoints\n",
		len(reg.Pages), reg.FromCache, reg.Fetched, reg.Entries, len(targets))

	// A partial registry walk yields a census of an arbitrary alphabetical slice
	// of the ecosystem, which looks exactly like a real result. Refuse by default:
	// silently publishing a truncated reading is the worst outcome this crawler
	// can produce.
	if !reg.Complete {
		meta.Notes = "PARTIAL registry walk: " + reg.StoppedAt
		fmt.Fprintf(os.Stderr, "\n  !! registry walk incomplete: %s\n", reg.StoppedAt)
		fmt.Fprintf(os.Stderr, "     %d pages are cached; re-run to resume from there.\n", len(reg.Pages))
		if !*allowPartial {
			_ = st.WriteMeta(meta)
			fatal("refusing to census a partial registry (pass --allow-partial to override)")
		}
		fmt.Fprintf(os.Stderr, "     --allow-partial set: continuing on a partial reading\n\n")
	}

	// ---------------------------------------------------------- selection
	if len(alreadyObserved) > 0 {
		kept := targets[:0]
		for _, t := range targets {
			if !alreadyObserved[t.Name+"\x00"+t.Endpoint] {
				kept = append(kept, t)
			}
		}
		fmt.Fprintf(os.Stderr, "  %d already observed, %d left to probe\n",
			len(targets)-len(kept), len(kept))
		targets = kept
	}
	if *onlyHost != "" {
		var keep []registry.Entry
		for _, t := range targets {
			if t.Host == *onlyHost {
				keep = append(keep, t)
			}
		}
		targets = keep
	}
	if *sample > 0 && *sample < len(targets) {
		r := rand.New(rand.NewSource(*seed))
		r.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
		targets = targets[:*sample]
	}
	if *limit > 0 && *limit < len(targets) {
		targets = targets[:*limit]
	}
	// What this run is meant to cover in total: what a resume already has, plus
	// what is still to do. The completeness flag is judged against this.
	intendedTotal := len(alreadyObserved) + len(targets)
	meta.Targets = intendedTotal

	if *dryRun {
		meta.Complete = reg.Complete
		if err := st.WriteMeta(meta); err != nil {
			fatal("meta: %v", err)
		}
		fmt.Fprintf(os.Stderr, "dry run: %d targets, registry archived, nothing probed\n", len(targets))
		return
	}

	// ---------------------------------------------------------- probing
	// Group by host so each host gets exactly one worker, then hand the hosts
	// out largest-first: the 1300-endpoint operators must start immediately or
	// they become the tail that decides how long the whole run takes.
	byHost := map[string][]registry.Entry{}
	for _, t := range targets {
		byHost[t.Host] = append(byHost[t.Host], t)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return len(byHost[hosts[i]]) > len(byHost[hosts[j]]) })

	fmt.Fprintf(os.Stderr, "probing %d endpoints across %d hosts with %d workers\n",
		len(targets), len(hosts), *workers)

	pr := probe.New(probe.Config{
		UserAgent:        userAgent,
		Timeout:          *timeout,
		ProtocolVersions: probe.DefaultConfig(userAgent).ProtocolVersions,
	})

	var (
		done     atomic.Int64
		toolsTot atomic.Int64
		outMu    sync.Mutex
		outcomes = map[string]int{}
	)

	queue := make(chan string, len(hosts))
	for _, h := range hosts {
		queue <- h
	}
	close(queue)

	// Progress has to suit its reader. On a terminal, a line that rewrites itself
	// with \r is right. Under systemd it is not: the journal cannot render a
	// carriage return, so an hour-long run logs nothing at all after "probing
	// ..." and looks exactly like a hang -- which is how the first server
	// deployment of this crawler was reported as frozen.
	interval, rewrite := 60*time.Second, false
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		interval, rewrite = 5*time.Second, true
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			d, t := done.Load(), toolsTot.Load()
			if rewrite {
				fmt.Fprintf(os.Stderr, "\r  %d/%d observed, %d tools", d, len(targets), t)
				continue
			}
			pct := 0.0
			if len(targets) > 0 {
				pct = 100 * float64(d) / float64(len(targets))
			}
			fmt.Fprintf(os.Stderr, "progress: %d/%d endpoints (%.1f%%), %d tools\n",
				d, len(targets), pct, t)
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for host := range queue {
				for i, t := range byHost[host] {
					if ctx.Err() != nil {
						return
					}
					if i > 0 {
						select {
						case <-ctx.Done():
							return
						case <-time.After(*delay):
						}
					}
					rec := observe(ctx, pr, st, runID, t)
					if err := st.Append(rec); err != nil {
						fmt.Fprintf(os.Stderr, "\nappend: %v\n", err)
					}
					done.Add(1)
					toolsTot.Add(int64(rec.ToolCount))
					outMu.Lock()
					outcomes[rec.Outcome]++
					outMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	ticker.Stop()
	fmt.Fprintln(os.Stderr)

	// Counted from the index, not from the in-memory counters: on a resumed run
	// those only cover what was probed since the resume.
	sum, err := store.SummarizeRun(*dataDir, runID)
	if err != nil {
		fatal("summarize: %v", err)
	}
	_ = outcomes // superseded by the index pass; kept for the live ticker only

	meta.Targets = sum.Records
	meta.Outcomes = sum.Outcomes
	meta.Tools = sum.Tools
	meta.Complete = ctx.Err() == nil && sum.Records >= intendedTotal
	if !meta.Complete {
		meta.Notes = fmt.Sprintf("incomplete: %d of %d endpoints observed", sum.Records, intendedTotal)
	}
	if err := st.WriteMeta(meta); err != nil {
		fatal("meta: %v", err)
	}

	// ---------------------------------------------------------- summary
	keys := make([]string, 0, len(sum.Outcomes))
	for k := range sum.Outcomes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return sum.Outcomes[keys[i]] > sum.Outcomes[keys[j]] })
	fmt.Fprintf(os.Stderr, "\nrun %s finished in %s\n", runID, time.Since(started).Round(time.Second))
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, "  %-16s %6d  %5.1f%%\n", k, sum.Outcomes[k],
			100*float64(sum.Outcomes[k])/float64(sum.Records))
	}
	fmt.Fprintf(os.Stderr, "  %-16s %6d\n", "tools observed", sum.Tools)
	fmt.Fprintf(os.Stderr, "\nindex: %s/runs/%s/index.jsonl\n", *dataDir, runID)
}

// observe probes one endpoint and turns the result into a record, archiving
// every byte received on the way.
func observe(ctx context.Context, pr *probe.Prober, st *store.Store, runID string, t registry.Entry) *store.Record {
	res := pr.Probe(ctx, t.Endpoint, t.Type)

	rec := &store.Record{
		RunID:           runID,
		ObservedAt:      time.Now().UTC().Format(time.RFC3339),
		ServerName:      t.Name,
		ServerVersion:   t.Version,
		Endpoint:        t.Endpoint,
		Host:            t.Host,
		DeclaredType:    t.Type,
		Outcome:         string(res.Outcome),
		Transport:       res.Transport,
		Handshake:       res.Handshake,
		HTTPStatus:      res.HTTPStatus,
		ProtocolVersion: res.ProtocolVersion,
		FailReason:      res.FailReason,
		DurationMS:      res.Elapsed.Milliseconds(),
		PageCount:       res.PageCount,
		Truncated:       res.Truncated,
	}

	if h, err := st.PutBlob(res.InitRaw); err == nil {
		rec.InitBlob = h
	}
	for _, p := range res.RawPages {
		if h, err := st.PutBlob(p); err == nil && h != "" {
			rec.PageBlobs = append(rec.PageBlobs, h)
		}
	}

	if res.Outcome != probe.OutcomeOK {
		return rec
	}

	surf, err := canon.SurfaceOf(res.ToolPages)
	if err != nil {
		// A tool that will not canonicalize is a finding, not an error to hide.
		// The raw bytes are already archived above.
		rec.SurfaceError = err.Error()
		for _, p := range res.ToolPages {
			rec.ToolCount += len(p)
		}
		return rec
	}
	rec.SurfaceSHA256 = surf.SurfaceSHA256
	rec.ToolCount = len(surf.Tools)
	rec.ToolNames = surf.ToolNames
	countAnnotations(surf.Tools, rec)
	return rec
}

// countAnnotations tallies the four MCP tool annotation hints. This is a derived
// view kept on the index line for cheap querying; the rule is a plain field
// presence check that a human can re-run against the archived blobs.
func countAnnotations(tools []json.RawMessage, rec *store.Record) {
	for _, raw := range tools {
		var t struct {
			Annotations map[string]json.RawMessage `json:"annotations"`
		}
		if json.Unmarshal(raw, &t) != nil || len(t.Annotations) == 0 {
			continue
		}
		hit := false
		if _, ok := t.Annotations["readOnlyHint"]; ok {
			rec.ReadOnlyHint++
			hit = true
		}
		if _, ok := t.Annotations["destructiveHint"]; ok {
			rec.DestructiveHnt++
			hit = true
		}
		if _, ok := t.Annotations["idempotentHint"]; ok {
			rec.IdempotentHint++
			hit = true
		}
		if _, ok := t.Annotations["openWorldHint"]; ok {
			rec.OpenWorldHint++
			hit = true
		}
		if hit {
			rec.Annotated++
		}
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "crawler: "+format+"\n", a...)
	os.Exit(1)
}
