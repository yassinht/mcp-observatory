package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/yhouta/mcp-observatory/internal/store"
)

// A "silent change" is a server that served the same set of tool names on two
// days with different content underneath -- an edited description or schema.
// That is the shape of a rug pull, and 95% of all observed changes have it, so
// the number carries the project.
//
// It is also the number most likely to be wrong. Some servers embed live data
// in a description ("cache updated 2026-08-30 09:14:02, 1,204 cities") which
// changes on every request and means nothing. Publishing those as changes would
// be a straightforward mistake, so this command separates them by a rule a
// human can re-run against the archived bytes: replace every digit with '#'
// and compare again. If the two texts then match, only numbers moved.
//
// No model, no heuristic, no judgement call. That constraint is deliberate --
// a transparency log whose classifications cannot be independently reproduced
// is just an opinion with extra steps.

type classCounts struct {
	description int
	schema      int
	volatile    int
	noBlob      int
	noDiff      int
}

type classExample struct {
	server, tool, before, after string
}

func cmdClassify(dir, a, b string) error {
	oldObs, err := loadRun(dir, a)
	if err != nil {
		return err
	}
	newObs, err := loadRun(dir, b)
	if err != nil {
		return err
	}

	var (
		c        classCounts
		total    int
		examples []classExample
	)
	// Counting servers overstates how much of the ecosystem moved. One operator
	// publishing forty servers from one template produces forty "changes" from a
	// single edit, and the first run of this command showed exactly that: four
	// io.github.mcp-dir servers with byte-identical edits to the same tool.
	// Both units are reported, because they answer different questions.
	realByHost := map[string]int{}
	realByOwner := map[string]int{}

	for k, o := range oldObs {
		n, ok := newObs[k]
		if !ok || o.Outcome != "ok" || n.Outcome != "ok" {
			continue
		}
		if o.Surface == n.Surface || !sameNames(o.ToolNames, n.ToolNames) {
			continue
		}
		total++

		oldTools, err1 := toolsOf(dir, o.PageBlobs)
		newTools, err2 := toolsOf(dir, n.PageBlobs)
		if err1 != nil || err2 != nil || len(oldTools) == 0 || len(newTools) == 0 {
			c.noBlob++
			continue
		}

		kind, tool, before, aft := classifyServer(oldTools, newTools)
		switch kind {
		case "description":
			c.description++
			if len(examples) < 5 {
				examples = append(examples, classExample{o.Server, tool, before, aft})
			}
		case "schema":
			c.schema++
		case "volatile":
			c.volatile++
		default:
			c.noDiff++
		}
		if kind == "description" || kind == "schema" {
			realByHost[o.Host]++
			realByOwner[ownerOf(o.Server)]++
		}
	}

	inspected := c.description + c.schema + c.volatile + c.noDiff
	fmt.Printf("%s -> %s\n\n", a, b)
	fmt.Printf("  silent changes (same tool names, different content)  %6d\n", total)
	fmt.Printf("  inspectable against archived bytes                   %6d\n\n", inspected)
	if inspected == 0 {
		fmt.Printf("  no blobs available to inspect (%d unreadable)\n", c.noBlob)
		return nil
	}
	fmt.Printf("  %-26s %6d  %5.1f%%   real\n", "description rewritten", c.description, pct(c.description, inspected))
	fmt.Printf("  %-26s %6d  %5.1f%%   real\n", "schema changed", c.schema, pct(c.schema, inspected))
	fmt.Printf("  %-26s %6d  %5.1f%%   FALSE POSITIVE\n", "digits only (volatile)", c.volatile, pct(c.volatile, inspected))
	if c.noDiff > 0 {
		fmt.Printf("  %-26s %6d  %5.1f%%\n", "no visible difference", c.noDiff, pct(c.noDiff, inspected))
	}
	if c.noBlob > 0 {
		fmt.Printf("  %-26s %6d          (not counted above)\n", "blob unreadable", c.noBlob)
	}

	real := c.description + c.schema
	fmt.Printf("\n  corrected silent-change rate: %.1f%% of inspected are real edits\n", pct(real, inspected))

	// How much of that is independent activity, and how much is one template?
	fmt.Printf("\n  %d real edits across %d hosts and %d registry owners\n",
		real, len(realByHost), len(realByOwner))
	if top := topN(realByOwner, 6); len(top) > 0 {
		covered := 0
		for _, kv := range top {
			covered += kv.n
		}
		fmt.Printf("  top owners (%.1f%% of all real edits):\n", pct(covered, real))
		for _, kv := range top {
			fmt.Printf("    %5d  %-46s %5.1f%%\n", kv.n, kv.k, pct(kv.n, real))
		}
	}

	if len(examples) > 0 {
		fmt.Printf("\nexamples of rewritten descriptions:\n")
		for _, e := range examples {
			bw, aw := window(e.before, e.after, 110)
			fmt.Printf("\n  %s / %s\n", e.server, e.tool)
			fmt.Printf("    before: %s\n", bw)
			fmt.Printf("    after:  %s\n", aw)
		}
	}
	return nil
}

// classifyServer reports the strongest signal found across a server's tools:
// a rewritten description outranks a schema change, which outranks digits.
func classifyServer(oldT, newT map[string]json.RawMessage) (kind, tool, before, aft string) {
	names := make([]string, 0, len(oldT))
	for n := range oldT {
		names = append(names, n)
	}
	sort.Strings(names)

	kind = ""
	for _, name := range names {
		o, n := oldT[name], newT[name]
		if n == nil || string(o) == string(n) {
			continue
		}
		od, nd := descriptionOf(o), descriptionOf(n)
		if od != nd {
			if normalizeDigits(od) == normalizeDigits(nd) {
				if kind == "" {
					kind = "volatile"
				}
				continue
			}
			return "description", name, od, nd
		}
		// Same description, different bytes: the schema or annotations moved.
		if normalizeDigits(string(o)) == normalizeDigits(string(n)) {
			if kind == "" {
				kind = "volatile"
			}
			continue
		}
		if kind != "description" {
			kind = "schema"
			tool = name
		}
	}
	return kind, tool, "", ""
}

func descriptionOf(raw json.RawMessage) string {
	var t struct {
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &t)
	return t.Description
}

// normalizeDigits replaces every digit with '#', so two texts that differ only
// in a timestamp, counter or rotating id compare equal.
func normalizeDigits(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= '0' && b[i] <= '9' {
			b[i] = '#'
		}
	}
	return string(b)
}

// toolsOf reads the archived tools/list responses and returns tools by name.
// It accepts both response shapes MCP servers use: a plain JSON body, and an
// SSE stream whose events may split one JSON message across several data:
// lines. Handling only the first shape is why an earlier analysis could inspect
// just 67 of 1,355 candidates.
func toolsOf(dir string, blobs []string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, h := range blobs {
		body, err := store.ReadBlob(dir, h)
		if err != nil {
			return nil, err
		}
		for _, msg := range jsonMessages(body) {
			var r struct {
				Result struct {
					Tools []json.RawMessage `json:"tools"`
				} `json:"result"`
			}
			if json.Unmarshal(msg, &r) != nil {
				continue
			}
			for _, t := range r.Result.Tools {
				var nm struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(t, &nm) == nil && nm.Name != "" {
					out[nm.Name] = t
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tools found in %d blob(s)", len(blobs))
	}
	return out, nil
}

// jsonMessages pulls every JSON-RPC message out of a stored response body.
func jsonMessages(body []byte) [][]byte {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		return [][]byte{[]byte(trimmed)}
	}

	var out [][]byte
	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		joined := strings.Join(data, "\n")
		data = nil
		if strings.HasPrefix(strings.TrimSpace(joined), "{") {
			out = append(out, []byte(joined))
		}
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line[5:], " "), " "))
		}
	}
	flush()
	return out
}

// ownerOf takes the publisher part of a registry name: everything before the
// first slash. "io.github.mcp-dir/caixa-mcp" and "io.github.mcp-dir/bmg-mcp"
// are two entries from one publisher, and counting them as two independent
// changes would double-count a single template edit.
func ownerOf(serverName string) string {
	if i := strings.IndexByte(serverName, '/'); i > 0 {
		return serverName[:i]
	}
	return serverName
}

type kv struct {
	k string
	n int
}

func topN(m map[string]int, n int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// window shows the region around the first difference between two texts.
// Printing the opening characters instead is useless: on a long description the
// edit is usually far from the start, so both lines look identical and the
// reader cannot see what actually changed.
func window(before, after string, n int) (string, string) {
	b := strings.Join(strings.Fields(before), " ")
	a := strings.Join(strings.Fields(after), " ")
	i := 0
	for i < len(b) && i < len(a) && b[i] == a[i] {
		i++
	}
	start := i - n/3
	if start < 0 {
		start = 0
	}
	cut := func(s string) string {
		if start >= len(s) {
			return ""
		}
		end := start + n
		if end > len(s) {
			end = len(s)
		}
		out := s[start:end]
		if start > 0 {
			out = "..." + out
		}
		if end < len(s) {
			out += "..."
		}
		return out
	}
	return cut(b), cut(a)
}
