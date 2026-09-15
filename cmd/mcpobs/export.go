package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/yassinht/mcp-transparency-log/internal/store"
)

// export writes the small derived files a static site can read.
//
// The archive is gigabytes and lives on the crawler's disk; GitHub Pages holds
// about a gigabyte and runs nothing. So the site gets a derivation, never the
// record itself. That split is deliberate rather than a limitation: a page
// built from derived data cannot be mistaken for the log, and the log stays
// something you verify with `mcpobs verify` against signed heads rather than
// something you read off a web page and hope is honest.
//
// Everything written here is recomputable from the archive. If a number on the
// site is ever disputed, the answer is to re-run this command, not to trust it.

type censusDay struct {
	Run       string         `json:"run"`
	Date      string         `json:"date"`
	Endpoints int            `json:"endpoints"`
	Outcomes  map[string]int `json:"outcomes"`
	Tools     int            `json:"tools"`
	Complete  bool           `json:"complete"`
}

type serverRow struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Endpoint  string `json:"endpoint"`
	Outcome   string `json:"outcome"`
	Tools     int    `json:"tools"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Days      int    `json:"days"`
	Changes   int    `json:"changes"`
	Surface   string `json:"surface,omitempty"`
}

func cmdExport(dataDir, outDir string) error {
	runs, err := runIDs(dataDir)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("no runs in %s", dataDir)
	}
	if err := os.MkdirAll(filepath.Join(outDir, "data"), 0o755); err != nil {
		return err
	}

	// ------------------------------------------------------------- census
	var census []censusDay
	for _, id := range runs {
		s, serr := store.SummarizeRun(dataDir, id)
		if serr != nil {
			continue
		}
		complete := false
		if m, merr := readMeta(dataDir, id); merr == nil {
			complete = m.Complete
		}
		census = append(census, censusDay{
			Run:       id,
			Date:      id[:10],
			Endpoints: s.Records,
			Outcomes:  s.Outcomes,
			Tools:     s.Tools,
			Complete:  complete,
		})
	}
	if err := writeJSON(filepath.Join(outDir, "data", "census.json"), census); err != nil {
		return err
	}
	fmt.Printf("census.json      %d days\n", len(census))

	// ------------------------------------------------------- servers + changes
	// One pass per run, carrying forward what each endpoint looked like last
	// time. Holding every run in memory at once would not survive a year of
	// this; holding one run plus a running summary will.
	type state struct {
		row     serverRow
		surface string
	}
	seen := map[string]*state{}

	for i, id := range runs {
		obs, lerr := loadRun(dataDir, id)
		if lerr != nil {
			continue
		}
		date := id[:10]
		var dayChanges []map[string]any

		for key, o := range obs {
			st, known := seen[key]
			if !known {
				seen[key] = &state{
					row: serverRow{
						Name: o.Server, Host: o.Host, Endpoint: o.Endpoint,
						Outcome: o.Outcome, Tools: o.ToolCount,
						FirstSeen: date, LastSeen: date, Days: 1,
					},
					surface: o.Surface,
				}
				// A server's first appearance is only newsworthy once the log has
				// a baseline; on day one everything is "new" and means nothing.
				if i > 0 {
					dayChanges = append(dayChanges, map[string]any{
						"kind": "appeared", "server": o.Server, "host": o.Host,
						"tools": o.ToolCount,
					})
				}
				continue
			}

			st.row.LastSeen = date
			st.row.Days++
			st.row.Outcome = o.Outcome
			st.row.Tools = o.ToolCount

			if o.Outcome != "ok" {
				if st.surface != "" {
					dayChanges = append(dayChanges, map[string]any{
						"kind": "unreachable", "server": o.Server, "host": o.Host,
						"outcome": o.Outcome,
					})
					st.surface = ""
				}
				continue
			}
			if st.surface == "" {
				st.surface = o.Surface
				continue
			}
			if o.Surface != st.surface {
				st.row.Changes++
				dayChanges = append(dayChanges, map[string]any{
					"kind": "surface_changed", "server": o.Server, "host": o.Host,
					"tools": o.ToolCount,
				})
				st.surface = o.Surface
			}
		}

		if i > 0 {
			sort.Slice(dayChanges, func(a, b int) bool {
				return fmt.Sprint(dayChanges[a]["server"]) < fmt.Sprint(dayChanges[b]["server"])
			})
			p := filepath.Join(outDir, "data", "changes")
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			if err := writeJSON(filepath.Join(p, date+".json"), dayChanges); err != nil {
				return err
			}
		}
	}

	rows := make([]serverRow, 0, len(seen))
	for _, st := range seen {
		st.row.Surface = st.surface
		rows = append(rows, st.row)
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
	if err := writeJSON(filepath.Join(outDir, "data", "servers.json"), rows); err != nil {
		return err
	}
	fmt.Printf("servers.json     %d endpoints\n", len(rows))
	fmt.Printf("changes/         %d days\n", len(runs)-1)
	fmt.Printf("\nwrote to %s\n", outDir)
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
