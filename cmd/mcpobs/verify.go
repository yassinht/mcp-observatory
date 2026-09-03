package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yhouta/mcp-observatory/internal/mlog"
)

// verify is the command this project exists to make possible. Everything else
// -- the crawler, the archive, the classifier -- produces claims. This is the
// part that lets a stranger check them without trusting the person who made
// them, which is the only reason a log is worth more than a spreadsheet.
//
// It deliberately recomputes the tree from the observation records rather than
// reading the stored hash file. Verifying a log against hashes the same
// operator wrote proves nothing: the hashes and the lie would be produced by
// the same hand. Rebuilding from the records and arriving at the signed root is
// the check that has teeth.
func cmdVerify(dataDir, logDir, headsDir, pubPath string) error {
	pub, err := mlog.LoadPublicKey(pubPath)
	if err != nil {
		return fmt.Errorf("public key: %w", err)
	}

	heads, err := loadHeads(headsDir)
	if err != nil {
		return err
	}
	if len(heads) == 0 {
		return fmt.Errorf("no signed heads found in %s", headsDir)
	}

	fmt.Printf("recomputing the tree from the observation records...\n")
	size, root, err := mlog.Recompute(logDir, dataDir)
	if err != nil {
		return fmt.Errorf("recompute: %w", err)
	}
	fmt.Printf("  %d observations\n  root %s\n\n", size, root)

	// Every head must verify under the key, and every head whose size we can
	// still reach must match what the records actually produce.
	var bad int
	for _, h := range heads {
		if err := h.head.Verify(pub); err != nil {
			fmt.Printf("  FAIL  %s  signature does not verify\n", h.name)
			bad++
			continue
		}
		if h.head.Size > size {
			fmt.Printf("  FAIL  %s  claims %d observations, records hold %d\n",
				h.name, h.head.Size, size)
			bad++
			continue
		}
		if h.head.Size == size {
			if h.head.Root.String() != root {
				fmt.Printf("  FAIL  %s  root does not match the records\n", h.name)
				bad++
				continue
			}
		}
		fmt.Printf("  ok    %s  size %d\n", h.name, h.head.Size)
	}

	// Heads must never move backwards: a smaller tree published after a larger
	// one means history was dropped, which no amount of valid signing excuses.
	for i := 1; i < len(heads); i++ {
		if heads[i].head.Size < heads[i-1].head.Size {
			fmt.Printf("\n  FAIL  %s shrinks the log from %d to %d observations\n",
				heads[i].name, heads[i-1].head.Size, heads[i].head.Size)
			bad++
		}
	}

	fmt.Println()
	if bad > 0 {
		return fmt.Errorf("%d head(s) failed verification", bad)
	}
	fmt.Printf("VERIFIED  %d heads, %d observations, nothing rewritten\n", len(heads), size)
	return nil
}

type namedHead struct {
	name string
	head *mlog.Head
}

func loadHeads(dir string) ([]namedHead, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("heads directory %s does not exist", dir)
		}
		return nil, err
	}
	var out []namedHead
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		h, err := mlog.ParseHead(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, namedHead{e.Name(), h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// cmdHeads lists the published heads, newest last, so a reader can see the log
// growing and spot a day that is missing.
func cmdHeads(headsDir string) error {
	heads, err := loadHeads(headsDir)
	if err != nil {
		return err
	}
	if len(heads) == 0 {
		fmt.Println("no heads published yet")
		return nil
	}
	fmt.Printf("%-22s %12s  %s\n", "head", "size", "root")
	prev := int64(0)
	for _, h := range heads {
		delta := ""
		if prev > 0 {
			delta = fmt.Sprintf("  (+%d)", h.head.Size-prev)
		}
		fmt.Printf("%-22s %12d  %s%s\n", h.name, h.head.Size, h.head.Root.String()[:16]+"...", delta)
		prev = h.head.Size
	}
	return nil
}
