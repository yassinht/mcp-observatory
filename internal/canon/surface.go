package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Surface is the thing this project actually watches: the set of tools a server
// served at one moment, reduced to a form where "same surface" means "same
// bytes".
//
// Two hashes are kept deliberately:
//
//	BodySHA256    hash of the exact response bytes. Provenance. Changes if the
//	              server so much as reorders a key, which is useless for change
//	              detection but essential for proving what was received.
//	SurfaceSHA256 hash of the canonical, name-sorted tool array. This is the
//	              change-detection key, and later the Merkle log leaf.
//
// Collapsing these two into one hash is the mistake that would make the log
// either noisy (raw only) or unprovable (canonical only).
type Surface struct {
	Tools         []json.RawMessage
	SurfaceSHA256 string
	ToolNames     []string
}

// SurfaceOf builds the canonical surface from the accumulated tools arrays of
// one or more tools/list pages.
func SurfaceOf(pages [][]json.RawMessage) (*Surface, error) {
	var all []json.RawMessage
	for _, p := range pages {
		all = append(all, p...)
	}

	type entry struct {
		name  string
		canon []byte
	}
	entries := make([]entry, 0, len(all))
	for i, raw := range all {
		c, err := Canonicalize(raw)
		if err != nil {
			return nil, fmt.Errorf("tool %d: %w", i, err)
		}
		// Pull the name out for sorting. A tool without a string name is a spec
		// violation, but it is a violation worth recording rather than dropping,
		// so it sorts under the empty string.
		var probe struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &probe)
		entries = append(entries, entry{name: probe.Name, canon: c})
	}

	// Sort by name, then by canonical bytes so that duplicate names still yield
	// a deterministic order instead of depending on the server's whim.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			a, b := utf16Of(entries[i].name), utf16Of(entries[j].name)
			for n := 0; n < len(a) && n < len(b); n++ {
				if a[n] != b[n] {
					return a[n] < b[n]
				}
			}
			return len(a) < len(b)
		}
		return bytes.Compare(entries[i].canon, entries[j].canon) < 0
	})

	var buf bytes.Buffer
	buf.WriteByte('[')
	names := make([]string, 0, len(entries))
	out := make([]json.RawMessage, 0, len(entries))
	for i, e := range entries {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(e.canon)
		names = append(names, e.name)
		out = append(out, json.RawMessage(e.canon))
	}
	buf.WriteByte(']')

	sum := sha256.Sum256(buf.Bytes())
	return &Surface{
		Tools:         out,
		SurfaceSHA256: hex.EncodeToString(sum[:]),
		ToolNames:     names,
	}, nil
}

// SHA256Hex is the content address used by the blob store.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
