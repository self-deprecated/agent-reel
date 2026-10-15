// Package loopoc is the V9 loop-encoding proof of concept: a measurement and
// exactness harness for alternative encodings of the visual store's views
// table. Nothing here is used by production code. See README.md.
package loopoc

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// View is one row of the visual store's views table, plus the hash of the
// screen it references (read from the screens table). Field for field it is
// what visualstore.Reader can hand back for a sequence number.
type View struct {
	Seq           int64
	Segment       int
	Line          int64
	AtNs          int64
	ScreenID      int64
	ScreenHash    [32]byte
	CursorCol     int
	CursorRow     int
	CursorVisible bool
	Chain         [32]byte
}

// Corpus is one recording's views in seq order (Views[i].Seq == i+1) with the
// commit boundaries the production pipeline sealed them at.
type Corpus struct {
	Name string
	// Kind is "captured" (the real helper and store) or "synthetic".
	Kind string
	// Seed is the generator seed of the source stream.
	Seed uint64
	// SpanNs is the logical length of the source stream.
	SpanNs     int64
	Cols, Rows int // initial geometry
	Views      []View
	// Commits holds the committed views total after each acknowledged batch
	// that added views: strictly increasing, the last equal to len(Views).
	Commits []int64
	// Tail is only set on a crash prefix (see Prefixes): views that were
	// appended after the last commit and lost with the open batch. Measure
	// appends them without a Commit; the durable result must still be
	// exactly Views.
	Tail []View
}

// ErrNoView is returned by Decoder.SeekTime when no view of the segment is at
// or before the time (visualstore.ErrNoView's meaning).
var ErrNoView = errors.New("loopoc: no view at or before that time")

// ChainSeed is the chain value the first view's chain is computed from
// (visualstore/canonical.go: chainSeed, 32 zero bytes).
var ChainSeed [32]byte

// ChainOf is a verbatim copy of visualstore.chainHash
// (internal/visualstore/canonical.go:46), with the arguments taken from a View:
// SHA-256 over the previous chain, segment, line, at_ns, screen hash, cursor
// column, cursor row and the visibility byte. TestChainOracle proves it
// reproduces every stored chain of captured corpora.
func ChainOf(prev [32]byte, v View) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	var scratch [8]byte
	for _, x := range []int64{int64(v.Segment), v.Line, v.AtNs} {
		binary.LittleEndian.PutUint64(scratch[:], uint64(x))
		h.Write(scratch[:])
	}
	h.Write(v.ScreenHash[:])
	for _, x := range []int64{int64(v.CursorCol), int64(v.CursorRow)} {
		binary.LittleEndian.PutUint64(scratch[:], uint64(x))
		h.Write(scratch[:])
	}
	visible := byte(0)
	if v.CursorVisible {
		visible = 1
	}
	h.Write([]byte{visible})
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// CheckChains returns the first seq whose Chain is not ChainOf(previous chain,
// view), or 0 when every chain links.
func CheckChains(views []View) int64 {
	prev := ChainSeed
	for _, v := range views {
		if ChainOf(prev, v) != v.Chain {
			return v.Seq
		}
		prev = v.Chain
	}
	return 0
}

// ViewsHash is a deterministic SHA-256 over every field of every view.
func ViewsHash(views []View) [32]byte {
	h := sha256.New()
	var b [8]byte
	put := func(x int64) {
		binary.LittleEndian.PutUint64(b[:], uint64(x))
		h.Write(b[:])
	}
	for _, v := range views {
		put(v.Seq)
		put(int64(v.Segment))
		put(v.Line)
		put(v.AtNs)
		put(v.ScreenID)
		h.Write(v.ScreenHash[:])
		put(int64(v.CursorCol))
		put(int64(v.CursorRow))
		if v.CursorVisible {
			put(1)
		} else {
			put(0)
		}
		h.Write(v.Chain[:])
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// Validate checks the corpus invariants: contiguous seqs, commits strictly
// increasing and ending at len(Views), and linked chains.
func (c Corpus) Validate() error {
	for i, v := range c.Views {
		if v.Seq != int64(i+1) {
			return fmt.Errorf("corpus %s: view %d has seq %d", c.Name, i, v.Seq)
		}
	}
	last := int64(0)
	for _, x := range c.Commits {
		if x <= last {
			return fmt.Errorf("corpus %s: commits not strictly increasing at %d", c.Name, x)
		}
		last = x
	}
	if last != int64(len(c.Views)) {
		return fmt.Errorf("corpus %s: last commit %d, %d views", c.Name, last, len(c.Views))
	}
	if bad := CheckChains(c.Views); bad != 0 {
		return fmt.Errorf("corpus %s: chain does not link at seq %d", c.Name, bad)
	}
	return nil
}

// ScreenTable is the screen id → screen hash map of the screens table. It is
// content, stored once per distinct screen and outside the views bytes, so a
// decoder may use it (a production reader joins screens the same way).
type ScreenTable map[int64][32]byte

// Screens returns the screen table of the given views.
func Screens(views []View) ScreenTable {
	t := ScreenTable{}
	for _, v := range views {
		t[v.ScreenID] = v.ScreenHash
	}
	return t
}

// Prefixes returns the durable state after a crash at many points:
//
//   - one truncation at every commit boundary b (Views[:b], the commits up to
//     b, no Tail);
//   - up to 32 seeded crashes at non-boundary view counts k, where only the
//     committed batches survive: Views[:b] for the last boundary b <= k, and
//     Tail = Views[b:k], the views appended to the open batch that was lost.
//     One of them is always inside the first batch (b = 0, nothing durable).
func Prefixes(c Corpus) []Corpus {
	var out []Corpus
	for i, b := range c.Commits {
		p := c
		p.Name = fmt.Sprintf("%s@%d", c.Name, b)
		p.Views = c.Views[:b]
		p.Commits = c.Commits[:i+1]
		p.Tail = nil
		out = append(out, p)
	}
	n := int64(len(c.Views))
	if n == 0 {
		return out
	}
	boundary := map[int64]bool{}
	for _, b := range c.Commits {
		boundary[b] = true
	}
	rng := rand.New(rand.NewPCG(c.Seed, 0x9e3779b97f4a7c15))
	crashes := map[int64]bool{}
	if first := c.Commits[0]; first > 1 {
		crashes[1+rng.Int64N(first-1)] = true
	}
	for tries := 0; len(crashes) < 32 && tries < 256; tries++ {
		k := 1 + rng.Int64N(n)
		if !boundary[k] {
			crashes[k] = true
		}
	}
	ks := make([]int64, 0, len(crashes))
	for k := range crashes {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	for _, k := range ks {
		i := sort.Search(len(c.Commits), func(i int) bool { return c.Commits[i] > k }) // commits[:i] are <= k
		b := int64(0)
		if i > 0 {
			b = c.Commits[i-1]
		}
		p := c
		p.Name = fmt.Sprintf("%s@crash%d", c.Name, k)
		p.Views = c.Views[:b]
		p.Commits = c.Commits[:i]
		p.Tail = c.Views[b:k]
		out = append(out, p)
	}
	return out
}

// SaveCorpus writes c as gob to path.
func SaveCorpus(path string, c Corpus) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LoadCorpus reads a corpus written by SaveCorpus and validates it.
func LoadCorpus(path string) (Corpus, error) {
	f, err := os.Open(path)
	if err != nil {
		return Corpus{}, err
	}
	defer f.Close()
	var c Corpus
	if err := gob.NewDecoder(f).Decode(&c); err != nil {
		return Corpus{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.Validate()
}

// LoadCorpora loads every <dir>/*.bin, sorted by name.
func LoadCorpora(dir string) ([]Corpus, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Corpus
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		c, err := LoadCorpus(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReferenceSeek is the linear-model answer to Decoder.SeekTime over views in
// seq order: the seq of the last view (largest at_ns, then seq) of the segment
// with at_ns <= atNs, exactly visualstore's viewAtSQL ordering.
func ReferenceSeek(views []View, segment int, atNs int64) (int64, error) {
	best := int64(0)
	var bestAt int64
	// Views of a segment have nondecreasing at_ns in seq order, so a binary
	// search over one contiguous segment would do; the linear fallback keeps
	// this obviously right for multi-segment corpora.
	lo, hi := segmentRange(views, segment)
	if lo < hi {
		i := sort.Search(hi-lo, func(i int) bool { return views[lo+i].AtNs > atNs })
		if i > 0 {
			return views[lo+i-1].Seq, nil
		}
		return 0, ErrNoView
	}
	for _, v := range views {
		if v.Segment == segment && v.AtNs <= atNs && (best == 0 || v.AtNs > bestAt || (v.AtNs == bestAt && v.Seq > best)) {
			best, bestAt = v.Seq, v.AtNs
		}
	}
	if best == 0 {
		return 0, ErrNoView
	}
	return best, nil
}

// segmentRange returns [lo,hi) when the segment's views are one contiguous run
// with nondecreasing at_ns, else (0,0).
func segmentRange(views []View, segment int) (int, int) {
	lo := slices.IndexFunc(views, func(v View) bool { return v.Segment == segment })
	if lo < 0 {
		return 0, 0
	}
	hi := lo
	for hi < len(views) && views[hi].Segment == segment {
		if hi > lo && views[hi].AtNs < views[hi-1].AtNs {
			return 0, 0
		}
		hi++
	}
	for _, v := range views[hi:] {
		if v.Segment == segment {
			return 0, 0
		}
	}
	return lo, hi
}
