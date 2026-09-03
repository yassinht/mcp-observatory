// Command mcpobs reads the observation log.
//
// The crawler writes; this reads. Keeping them separate matters more than it
// looks: this is the binary other people run, so it must never be able to
// mutate the log it is reporting on.
//
//	mcpobs runs                     list observation runs
//	mcpobs diff <runA> <runB>       what changed between two runs
//	mcpobs show <server>            one server's history
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yhouta/mcp-observatory/internal/store"
)

func main() {
	dataDir := flag.String("data", "data", "observation log directory")
	logDir := flag.String("tlog", "", "merkle log directory (default <data>/tlog)")
	headsDir := flag.String("heads", "heads", "directory of signed tree heads")
	pubKey := flag.String("pubkey", "heads/key.pub", "public key that signs the heads")
	flag.Parse()

	if *logDir == "" {
		*logDir = filepath.Join(*dataDir, "tlog")
	}

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "runs":
		err = cmdRuns(*dataDir)
	case "diff":
		if len(args) != 3 {
			fatal("usage: mcpobs diff <runA> <runB>")
		}
		err = cmdDiff(*dataDir, args[1], args[2])
	case "classify":
		if len(args) != 3 {
			fatal("usage: mcpobs classify <runA> <runB>")
		}
		err = cmdClassify(*dataDir, args[1], args[2])
	case "show":
		if len(args) != 2 {
			fatal("usage: mcpobs show <server-name-substring>")
		}
		err = cmdShow(*dataDir, args[1])
	case "verify":
		err = cmdVerify(*dataDir, *logDir, *headsDir, *pubKey)
	case "heads":
		err = cmdHeads(*headsDir)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal("%v", err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mcpobs -- read the MCP Observatory log

  mcpobs runs                  list observation runs, newest first
  mcpobs diff <runA> <runB>    what changed between two runs
  mcpobs classify <runA> <runB>  split real edits from volatile noise
  mcpobs show <server>         one server's observed history
  mcpobs verify                rebuild the tree from the records and check
                               it against every signed head
  mcpobs heads                 list the published tree heads

flags:
  --data <dir>                 observation directory (default "data")
  --tlog <dir>                 merkle log (default <data>/tlog)
  --heads <dir>                signed heads (default "heads")
  --pubkey <file>              signing public key (default "heads/key.pub")
`)
}

// ---------------------------------------------------------------- runs

func cmdRuns(dir string) error {
	ids, err := runIDs(dir)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("no runs found in", filepath.Join(dir, "runs"))
		return nil
	}
	fmt.Printf("%-24s %8s %8s %10s  %s\n", "run", "records", "ok", "tools", "state")
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		m, merr := readMeta(dir, id)
		s, serr := store.SummarizeRun(dir, id)
		if serr != nil {
			continue
		}
		state := "incomplete"
		if merr == nil && m.Complete {
			state = "complete"
		} else if merr != nil {
			state = "no meta"
		}
		fmt.Printf("%-24s %8d %8d %10d  %s\n", id, s.Records, s.Outcomes["ok"], s.Tools, state)
	}
	return nil
}

// ---------------------------------------------------------------- diff

type obs struct {
	Server    string   `json:"server_name"`
	Endpoint  string   `json:"endpoint"`
	Outcome   string   `json:"outcome"`
	Surface   string   `json:"surface_sha256"`
	ToolNames []string `json:"tool_names"`
	ToolCount int      `json:"tool_count"`
	PageBlobs []string `json:"page_blobs"`
	Host      string   `json:"host"`
}

func cmdDiff(dir, a, b string) error {
	oldObs, err := loadRun(dir, a)
	if err != nil {
		return err
	}
	newObs, err := loadRun(dir, b)
	if err != nil {
		return err
	}

	var (
		bothOK, unchanged int
		changed           []diffEntry
		appeared, gone    []string
		lostAccess        []string
		gainedAccess      []string
	)

	for k, o := range oldObs {
		n, ok := newObs[k]
		if !ok {
			gone = append(gone, o.Server)
			continue
		}
		switch {
		case o.Outcome == "ok" && n.Outcome == "ok":
			bothOK++
			if o.Surface == n.Surface {
				unchanged++
				continue
			}
			changed = append(changed, diffEntry{
				server: o.Server,
				// added: present in the new run, absent from the old.
				added:   missing(o.ToolNames, n.ToolNames),
				removed: missing(n.ToolNames, o.ToolNames),
				oldN:    o.ToolCount,
				newN:    n.ToolCount,
			})
		case o.Outcome == "ok" && n.Outcome != "ok":
			lostAccess = append(lostAccess, o.Server+" -> "+n.Outcome)
		case o.Outcome != "ok" && n.Outcome == "ok":
			gainedAccess = append(gainedAccess, n.Server)
		}
	}
	for k, n := range newObs {
		if _, ok := oldObs[k]; !ok {
			appeared = append(appeared, n.Server)
		}
	}

	fmt.Printf("%s -> %s\n\n", a, b)
	fmt.Printf("  endpoints in both runs        %6d\n", len(oldObs)-len(gone))
	fmt.Printf("  enumerable in both            %6d\n", bothOK)
	fmt.Printf("    unchanged surface           %6d  %5.2f%%\n", unchanged, pct(unchanged, bothOK))
	fmt.Printf("    CHANGED surface             %6d  %5.2f%%\n", len(changed), pct(len(changed), bothOK))
	fmt.Printf("  newly listed                  %6d\n", len(appeared))
	fmt.Printf("  no longer listed              %6d\n", len(gone))
	fmt.Printf("  became unreachable            %6d\n", len(lostAccess))
	fmt.Printf("  became reachable              %6d\n", len(gainedAccess))

	if len(changed) == 0 {
		return nil
	}
	// Surface changes that keep the same tool names are the interesting ones:
	// a description or schema was edited in place, which is the rug-pull shape.
	sort.Slice(changed, func(i, j int) bool { return changed[i].server < changed[j].server })
	var silent []diffEntry
	fmt.Printf("\nchanged surfaces:\n")
	for _, c := range changed {
		if len(c.added) == 0 && len(c.removed) == 0 {
			silent = append(silent, c)
			continue
		}
		fmt.Printf("  %s  (%d -> %d tools)\n", c.server, c.oldN, c.newN)
		if len(c.added) > 0 {
			fmt.Printf("      + %s\n", strings.Join(clip(c.added, 8), ", "))
		}
		if len(c.removed) > 0 {
			fmt.Printf("      - %s\n", strings.Join(clip(c.removed, 8), ", "))
		}
	}
	if len(silent) > 0 {
		fmt.Printf("\nsame tool names, edited content (%d) -- descriptions or schemas changed in place:\n", len(silent))
		for _, c := range silent {
			fmt.Printf("  %s  (%d tools)\n", c.server, c.newN)
		}
	}
	return nil
}

type diffEntry struct {
	server         string
	added, removed []string
	oldN, newN     int
}

// ---------------------------------------------------------------- show

func cmdShow(dir, needle string) error {
	ids, err := runIDs(dir)
	if err != nil {
		return err
	}
	needle = strings.ToLower(needle)
	found := false
	var prev string
	for _, id := range ids {
		m, err := loadRun(dir, id)
		if err != nil {
			continue
		}
		for _, o := range m {
			if !strings.Contains(strings.ToLower(o.Server), needle) {
				continue
			}
			found = true
			mark := ""
			switch {
			case prev == "":
				mark = "first seen"
			case o.Surface != prev && o.Surface != "":
				mark = "CHANGED"
			default:
				mark = "no change"
			}
			short := o.Surface
			if len(short) > 12 {
				short = short[:12]
			}
			fmt.Printf("%-24s %-14s %4d tools  %-12s %s\n", id, o.Outcome, o.ToolCount, short, mark)
			if o.Surface != "" {
				prev = o.Surface
			}
			break // one endpoint per run is enough for a history view
		}
	}
	if !found {
		fmt.Printf("no server matching %q in the log\n", needle)
	}
	return nil
}

// ---------------------------------------------------------------- helpers

func loadRun(dir, id string) (map[string]obs, error) {
	f, err := os.Open(filepath.Join(dir, "runs", id, "index.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]obs{}
	dec := json.NewDecoder(f)
	for {
		var o obs
		if err := dec.Decode(&o); err != nil {
			break // truncated tail is tolerated; see store.ObservedEndpoints
		}
		if o.Endpoint != "" {
			out[o.Server+"\x00"+o.Endpoint] = o
		}
	}
	return out, nil
}

func runIDs(dir string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func readMeta(dir, id string) (*store.RunMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, "runs", id, "meta.json"))
	if err != nil {
		return nil, err
	}
	var m store.RunMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// missing returns the entries of want that are absent from have.
func missing(have, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	return out
}

func clip(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], fmt.Sprintf("... +%d more", len(s)-n))
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "mcpobs: "+format+"\n", a...)
	os.Exit(1)
}
