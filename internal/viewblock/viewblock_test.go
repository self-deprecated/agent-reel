package viewblock_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
)

// commitRuns splits c's views at its production commit boundaries, then at
// segment changes and at cap views: the blocks a store would write.
func commitRuns(c loopoc.Corpus, cap int) [][]viewblock.View {
	var out [][]viewblock.View
	prev := int64(0)
	for _, b := range c.Commits {
		for _, run := range splitRuns(c.Views[prev:b], cap) {
			out = append(out, toBlock(run))
		}
		prev = b
	}
	return out
}

func roundTrip(t *testing.T, name string, vs []viewblock.View) []byte {
	t.Helper()
	ref, payload, err := viewblock.Encode(vs)
	if err != nil {
		t.Fatalf("%s: encode seq %d+%d: %v", name, vs[0].Seq, len(vs), err)
	}
	want := viewblock.Ref{FirstSeq: vs[0].Seq, Count: len(vs), Segment: vs[0].Segment, FirstLine: vs[0].Line, FirstAtNs: vs[0].AtNs}
	if ref != want {
		t.Fatalf("%s: ref %+v, want %+v", name, ref, want)
	}
	got, err := viewblock.Decode(ref, payload)
	if err != nil {
		t.Fatalf("%s: decode seq %d+%d: %v", name, vs[0].Seq, len(vs), err)
	}
	if len(got) != len(vs) {
		t.Fatalf("%s: %d views decoded, %d encoded", name, len(got), len(vs))
	}
	for i := range vs {
		if got[i] != vs[i] {
			t.Fatalf("%s: seq %d decoded %+v, encoded %+v", name, vs[i].Seq, got[i], vs[i])
		}
	}
	return payload
}

// TestRoundTrip: Decode(Encode(vs)) == vs field by field on every synthetic
// corpus, split at the production commit boundaries and again at 1,024 views
// (one commit per corpus, the ideal policy under the live cap).
func TestRoundTrip(t *testing.T) {
	for _, c := range loopoc.SyntheticCorpora() {
		zstdBlocks, rawBlocks := 0, 0
		for _, vs := range commitRuns(c, viewblock.MaxViews) {
			if roundTrip(t, c.Name, vs)[0] == viewblock.FlagZstd {
				zstdBlocks++
			} else {
				rawBlocks++
			}
		}
		all := c
		all.Commits = []int64{int64(len(c.Views))}
		capped := commitRuns(all, liveCap)
		for _, vs := range capped {
			roundTrip(t, c.Name, vs)
		}
		t.Logf("%-14s %d commit blocks (%d zstd, %d raw), %d blocks at the 1024 cap", c.Name, zstdBlocks+rawBlocks, zstdBlocks, rawBlocks, len(capped))
	}
}

// TestRoundTripExtremes: values at the edges of every column survive, and
// Encode refuses a delta that would overflow instead of wrapping.
func TestRoundTripExtremes(t *testing.T) {
	vs := []viewblock.View{
		{Seq: 1, Segment: math.MaxInt32, Line: math.MinInt64, AtNs: math.MinInt64, ScreenID: math.MinInt64, CursorCol: math.MinInt, CursorRow: math.MaxInt, CursorVisible: true},
		{Seq: 2, Segment: math.MaxInt32, Line: -1, AtNs: -1, ScreenID: -1, CursorCol: -1, CursorRow: 0},
		{Seq: 3, Segment: math.MaxInt32, Line: math.MaxInt64 - 1, AtNs: math.MaxInt64 - 1, ScreenID: math.MaxInt64 - 1, CursorCol: math.MaxInt - 1, CursorRow: math.MinInt + 1, CursorVisible: true},
		{Seq: 4, Segment: math.MaxInt32, Line: math.MaxInt64, AtNs: math.MaxInt64, ScreenID: 0, CursorCol: 0, CursorRow: 0},
	}
	roundTrip(t, "extremes", vs)
	one := []viewblock.View{{Seq: math.MaxInt64, Segment: -3, Line: 5, AtNs: 7, ScreenID: 9}}
	roundTrip(t, "single", one)

	for name, bad := range map[string][]viewblock.View{
		"at_ns":      {{Seq: 1, AtNs: math.MinInt64}, {Seq: 2, AtNs: 1}},
		"line":       {{Seq: 1, Line: -2}, {Seq: 2, Line: math.MaxInt64}},
		"screen_id":  {{Seq: 1, ScreenID: math.MinInt64}, {Seq: 2, ScreenID: 1}},
		"cursor_col": {{Seq: 1, CursorCol: math.MinInt}, {Seq: 2, CursorCol: 1}},
	} {
		if _, _, err := viewblock.Encode(bad); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s overflow: %v", name, err)
		}
	}
}

// spinner is a 20-view spinner-shaped block (qualify W5): ten screens
// cycling, 100 ms apart, one source line each, the cursor at rest.
func spinner(first int64, n int) []viewblock.View {
	vs := make([]viewblock.View, n)
	for i := range vs {
		seq := first + int64(i)
		vs[i] = viewblock.View{Seq: seq, Segment: 1, Line: seq + 1, AtNs: seq * 100_000_000, ScreenID: 1 + seq%10, CursorCol: 1, CursorRow: 0, CursorVisible: true}
	}
	return vs
}

// TestEncodeRefuses: Encode never splits, reorders or accepts what Decode
// would not expand.
func TestEncodeRefuses(t *testing.T) {
	base := spinner(1, 20)
	mutate := func(i int, f func(*viewblock.View)) []viewblock.View {
		vs := slices.Clone(base)
		f(&vs[i])
		return vs
	}
	for name, c := range map[string]struct {
		views []viewblock.View
		want  string
	}{
		"empty":          {nil, "no views"},
		"seq gap":        {mutate(7, func(v *viewblock.View) { v.Seq++ }), "contiguous"},
		"seq repeat":     {mutate(7, func(v *viewblock.View) { v.Seq-- }), "contiguous"},
		"seq zero":       {spinner(0, 3), "outside"},
		"segment change": {mutate(12, func(v *viewblock.View) { v.Segment = 2 }), "segment"},
		"at backwards":   {mutate(5, func(v *viewblock.View) { v.AtNs = base[4].AtNs - 1 }), "goes back"},
		"line backwards": {mutate(5, func(v *viewblock.View) { v.Line = base[4].Line - 1 }), "goes back"},
	} {
		if _, _, err := viewblock.Encode(c.views); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, c.want)
		}
	}
	if _, _, err := viewblock.Encode(make([]viewblock.View, viewblock.MaxViews+1)); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Errorf("MaxViews+1 views: %v", err)
	}
}

// TestSegmentChangeIsAnEncodeError: a slice that changes segment mid-way is
// refused, however late the change; the store splits runs, the codec never
// does.
func TestSegmentChangeIsAnEncodeError(t *testing.T) {
	vs := toBlock(syntheticNamed(t, loopoc.S3Jitter).Views[:300])
	for _, at := range []int{1, 150, 299} {
		bad := slices.Clone(vs)
		for i := at; i < len(bad); i++ {
			bad[i].Segment = 2
		}
		if ref, payload, err := viewblock.Encode(bad); err == nil {
			t.Fatalf("a segment change at view %d was encoded: %+v, %d bytes", at, ref, len(payload))
		}
	}
}

// TestDecodeRefuses covers every refusal of Decode on hand-built payloads.
func TestDecodeRefuses(t *testing.T) {
	ref, payload, err := viewblock.Encode(spinner(1, 20))
	if err != nil || payload[0] != viewblock.FlagZstd {
		t.Fatalf("setup: %v flags %#x", err, payload[0])
	}
	rawRef, rawPayload, err := viewblock.Encode(spinner(1, 2))
	if err != nil || rawPayload[0] != 0 {
		t.Fatalf("setup: %v flags %#x", err, rawPayload[0])
	}
	rawCols := rawPayload[1:]
	withRef := func(f func(*viewblock.Ref)) viewblock.Ref { r := ref; f(&r); return r }
	cols := func(xs ...int64) []byte { // a raw payload of signed varints
		b := []byte{0}
		for _, x := range xs {
			b = binary.AppendVarint(b, x)
		}
		return b
	}
	z := func(raw []byte) []byte {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		return append([]byte{viewblock.FlagZstd}, enc.EncodeAll(raw, nil)...)
	}
	one := viewblock.Ref{FirstSeq: 1, Count: 1}
	two := viewblock.Ref{FirstSeq: 1, Count: 2}
	for name, c := range map[string]struct {
		ref     viewblock.Ref
		payload []byte
		want    string
	}{
		"count zero":        {withRef(func(r *viewblock.Ref) { r.Count = 0 }), payload, "count"},
		"count too large":   {withRef(func(r *viewblock.Ref) { r.Count = viewblock.MaxViews + 1 }), payload, "count"},
		"seq zero":          {withRef(func(r *viewblock.Ref) { r.FirstSeq = 0 }), payload, "outside"},
		"seq overflow":      {withRef(func(r *viewblock.Ref) { r.FirstSeq = math.MaxInt64 }), payload, "outside"},
		"empty":             {ref, nil, "empty"},
		"reserved flag":     {ref, append([]byte{viewblock.FlagZstd | 0x04}, payload[1:]...), "reserved"},
		"reserved raw flag": {rawRef, append([]byte{0x80}, rawCols...), "reserved"},
		"fewer views":       {withRef(func(r *viewblock.Ref) { r.Count = 19 }), payload, "trailing"},
		"more views":        {withRef(func(r *viewblock.Ref) { r.Count = 21 }), payload, "run out"},
		"raw trailing":      {rawRef, append(slices.Clone(rawPayload), 0), "trailing"},
		"raw run out":       {rawRef, rawPayload[:len(rawPayload)-1], "run out"},
		"raw beyond bound":  {one, append([]byte{0}, make([]byte, viewblock.MaxColumnBytes(1)+1)...), "bound"},
		"zstd garbage":      {ref, append([]byte{viewblock.FlagZstd}, 1, 2, 3, 4, 5, 6, 7, 8), "zstd"},
		"zstd beyond bound": {one, z(make([]byte, viewblock.MaxColumnBytes(1)+1)), "bound"},
		"zstd trailing":     {one, z(append(cols(0, 0, 0)[1:], 0, 7)), "trailing"},
		"at backwards":      {two, cols(-1, 0, 0, 0, 0, 0, 0, 0, 0, 0), "goes back"},
		"line backwards":    {two, cols(0, -1, 0, 0, 0, 0, 0, 0, 0, 0), "goes back"},
		"visible not 0/1":   {one, append(cols(0, 0, 0), 2), "not 0 or 1"},
		"non-minimal":       {one, []byte{0, 0x80, 0x00, 0, 0, 0}, "minimal"},
		"varint overflow":   {one, append([]byte{0}, bytes.Repeat([]byte{0xff}, 11)...), "overflows"},
		"screen overflow":   {two, cols(0, 0, math.MaxInt64, 1, 0, 0, 0, 0, 0, 0), "overflows"},
		"at overflow":       {viewblock.Ref{FirstSeq: 1, Count: 2, FirstAtNs: math.MaxInt64}, cols(1, 0, 0, 0, 0, 0, 0, 0, 0, 0), "overflows"},
	} {
		got, err := viewblock.Decode(c.ref, c.payload)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (%d views), want an error containing %q", name, err, len(got), c.want)
		}
	}
}

// TestDeterminismAndNoFutureRead: Encode of the same views is byte-identical
// across calls, and a block's bytes depend only on its own views: encoding
// vs[:k] from a slice that ends at k gives the same bytes as encoding it as a
// prefix of the full corpus, and Encode never modifies its input.
func TestDeterminismAndNoFutureRead(t *testing.T) {
	for _, c := range loopoc.SyntheticCorpora() {
		full := toBlock(c.Views)
		ks := []int{1, 2, 3, 20, 257, min(1024, len(full)), len(full)}
		before := make(map[int][]byte, len(ks))
		for _, k := range ks {
			// Built before the rest of the corpus exists: a fresh slice of k.
			alone := toBlock(c.Views[:k])
			_, p, err := viewblock.Encode(alone[:k:k])
			if err != nil {
				t.Fatal(err)
			}
			before[k] = p
		}
		snapshot := slices.Clone(full)
		for _, k := range ks {
			for range 2 {
				_, p, err := viewblock.Encode(full[:k])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(p, before[k]) {
					t.Fatalf("%s: the %d-view prefix encodes differently inside the full corpus", c.Name, k)
				}
			}
		}
		if !reflect.DeepEqual(full, snapshot) {
			t.Fatalf("%s: Encode modified its input", c.Name)
		}
	}
}

// corruptionPayloads returns one zstd and one raw payload with their views.
func corruptionPayloads(t *testing.T) map[string]struct {
	ref     viewblock.Ref
	payload []byte
	views   []viewblock.View
} {
	t.Helper()
	out := map[string]struct {
		ref     viewblock.Ref
		payload []byte
		views   []viewblock.View
	}{}
	// Compressed: a 256-view jittered block.
	vs := toBlock(syntheticNamed(t, loopoc.S6Mixed).Views[:256])
	ref, p, err := viewblock.Encode(vs)
	if err != nil || p[0] != viewblock.FlagZstd {
		t.Fatalf("zstd setup: %v flags %#x", err, p[0])
	}
	out["zstd"] = struct {
		ref     viewblock.Ref
		payload []byte
		views   []viewblock.View
	}{ref, p, vs}
	// Raw: 24 views whose columns are seeded noise, which zstd cannot shrink.
	rng := rand.New(rand.NewPCG(14, 1))
	vs = make([]viewblock.View, 24)
	for i := range vs {
		vs[i] = viewblock.View{Seq: 100 + int64(i), Segment: 3, Line: 10 + int64(i)*rng.Int64N(1<<20), AtNs: int64(i) * rng.Int64N(1<<40),
			ScreenID: rng.Int64N(1 << 30), CursorCol: rng.IntN(1 << 12), CursorRow: rng.IntN(1 << 12), CursorVisible: rng.IntN(2) == 0}
		if i > 0 {
			vs[i].Line = max(vs[i].Line, vs[i-1].Line)
			vs[i].AtNs = max(vs[i].AtNs, vs[i-1].AtNs)
		}
	}
	ref, p, err = viewblock.Encode(vs)
	if err != nil || p[0] != 0 {
		t.Fatalf("raw setup: %v flags %#x", err, p[0])
	}
	out["raw"] = struct {
		ref     viewblock.Ref
		payload []byte
		views   []viewblock.View
	}{ref, p, vs}
	return out
}

// TestCorruption: for 1,000 seeded single-byte mutations and truncations of a
// compressed and of a raw payload, Decode either refuses or returns views
// that differ from the encoded ones; it never claims the same views from
// different bytes. The second case (a clean decode to other views) is not the
// codec's to catch: the store recomputes the block's chains from the decoded
// views and compares its stored last chain, which refuses the block.
func TestCorruption(t *testing.T) {
	for name, c := range corruptionPayloads(t) {
		rng := rand.New(rand.NewPCG(0x1400, uint64(len(name))))
		refused, differ := 0, 0
		for i := range 1000 {
			var bad []byte
			var what string
			if i%4 == 3 {
				n := rng.IntN(len(c.payload))
				bad, what = c.payload[:n], "truncation to "+strconv.Itoa(n)
			} else {
				bad = slices.Clone(c.payload)
				at := rng.IntN(len(bad))
				bad[at] ^= byte(1 + rng.IntN(255))
				what = "byte " + strconv.Itoa(at)
			}
			got, err := viewblock.Decode(c.ref, bad)
			switch {
			case err != nil:
				refused++
			case slices.Equal(got, c.views):
				t.Fatalf("%s: %s decoded to the original views", name, what)
			default:
				differ++
			}
		}
		t.Logf("%s payload (%d bytes): %d refused, %d decoded to other views (the chain catches those)", name, len(c.payload), refused, differ)
	}
}

// TestBytesGuard: on the synthetic S1 (qualify W5's spinner) under the prod
// policy, the mean payload bytes per view stay within 20% of V9's a-blockz
// figure for the captured W5 corpus under the prod policy, 5.2048 B/view in
// scratchpad/loopoc/results/blockz.json. That figure is a whole record (V9
// header, chain and the harness's 16-byte record overhead), so the payload
// alone is well inside it; the harness figure for this codec (Ref columns,
// chain and overhead serialised) must be too.
func TestBytesGuard(t *testing.T) {
	const v9W5Prod, limit = 5.204799200133311, 5.204799200133311 * 1.2
	c := syntheticNamed(t, loopoc.S1Pure)
	var bytesTotal int
	blocks := commitRuns(c, liveCap)
	for _, vs := range blocks {
		_, p, err := viewblock.Encode(vs)
		if err != nil {
			t.Fatal(err)
		}
		bytesTotal += len(p)
	}
	perView := float64(bytesTotal) / float64(len(c.Views))
	r := loopoc.MeasurePolicy(c, harnessCodec{Cap: liveCap}, loopoc.PolicyProd)
	t.Logf("%s prod: %d views, %d blocks, payload %.3f B/view, harness record %.3f B/view (V9 a-blockz W5 %.3f, limit %.3f)",
		c.Name, len(c.Views), len(blocks), perView, r.BytesPerView, v9W5Prod, limit)
	if perView > limit || r.BytesPerView > limit {
		t.Fatalf("payload %.3f or record %.3f B/view exceeds %.3f", perView, r.BytesPerView, limit)
	}
}

func BenchmarkEncode(b *testing.B) {
	vs := spinner(1, 20)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := viewblock.Encode(vs); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(vs)), "ns/view")
}

func BenchmarkDecode(b *testing.B) {
	vs := spinner(1, 20)
	ref, p, err := viewblock.Encode(vs)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := viewblock.Decode(ref, p); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(vs)), "ns/view")
	b.ReportMetric(float64(len(p)), "payload-bytes")
}

// TestDecodeRefusesMoreThanOneFrame: a zstd body must be exactly one frame.
// DecodeAll alone decodes concatenated frames and skips skippable ones, so an
// empty second frame or a skippable frame after the first used to decode to
// the same views; with or without a frame content size, each is refused.
func TestDecodeRefusesMoreThanOneFrame(t *testing.T) {
	// A complete frame of no content: single segment, a one-byte content size
	// of 0, one last raw block of size 0.
	empty := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 0x00, 0x01, 0x00, 0x00}
	skippable := []byte{0x50, 0x2a, 0x4d, 0x18, 2, 0, 0, 0, 0xaa, 0xbb}
	for _, n := range []int{20, 400} {
		vs := spinner(1, n)
		ref, payload, err := viewblock.Encode(vs)
		if err != nil || payload[0] != viewblock.FlagZstd {
			t.Fatalf("%d views: %v flags %#x", n, err, payload[0])
		}
		var h zstd.Header
		if err := h.Decode(payload[1:]); err != nil {
			t.Fatal(err)
		}
		if h.HasFCS != (n == 400) {
			t.Fatalf("%d views: frame content size present %v, the test wants both shapes", n, h.HasFCS)
		}
		if got, err := viewblock.Decode(ref, payload); err != nil || !slices.Equal(got, vs) {
			t.Fatalf("%d views: the single frame does not round-trip: %v", n, err)
		}
		for name, tail := range map[string][]byte{
			"empty frame":     empty,
			"skippable frame": skippable,
			"same frame":      payload[1:],
			"one byte":        {0},
		} {
			bad := append(slices.Clone(payload), tail...)
			if got, err := viewblock.Decode(ref, bad); err == nil || !strings.Contains(err.Error(), "follow the zstd frame") {
				t.Errorf("%d views + %s: %v (%d views), want a trailing-frame refusal", n, name, err, len(got))
			}
		}
		for _, cut := range []int{1, 2, 4} {
			if _, err := viewblock.Decode(ref, payload[:len(payload)-cut]); err == nil {
				t.Errorf("%d views cut by %d: decoded", n, cut)
			}
		}
	}
}

// TestFrameContentSizeThreshold pins the inflate comment: Encode's frames
// carry no content size for column bytes under 256 and carry one from 256 on,
// so small commit blocks take inflate's bound-allocation path.
func TestFrameContentSizeThreshold(t *testing.T) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	seen := map[bool]bool{}
	for n := 1; n <= 200; n++ {
		_, payload, err := viewblock.Encode(spinner(1, n))
		if err != nil {
			t.Fatal(err)
		}
		if payload[0] != viewblock.FlagZstd {
			continue
		}
		raw, err := dec.DecodeAll(payload[1:], nil)
		if err != nil {
			t.Fatal(err)
		}
		var h zstd.Header
		if err := h.Decode(payload[1:]); err != nil {
			t.Fatal(err)
		}
		if h.HasFCS != (len(raw) >= 256) {
			t.Fatalf("%d views, %d column bytes: frame content size present %v", n, len(raw), h.HasFCS)
		}
		seen[h.HasFCS] = true
	}
	if !seen[true] || !seen[false] {
		t.Fatalf("both frame shapes should occur: %v", seen)
	}
}

// TestDecodeRefusesTooFewColumnBytes: MinColumnBytes is exact (a block of
// all-zero one-byte varints decodes, one byte fewer does not), and a damaged
// count beside a short payload is refused before ref.Count views are
// allocated.
func TestDecodeRefusesTooFewColumnBytes(t *testing.T) {
	for _, n := range []int{1, 2, 20, 1024} {
		if got := viewblock.MinColumnBytes(n); got != 2*(n-1)+4*n {
			t.Fatalf("MinColumnBytes(%d) = %d", n, got)
		}
		ref := viewblock.Ref{FirstSeq: 1, Count: n, FirstLine: 1}
		least := append([]byte{0}, make([]byte, viewblock.MinColumnBytes(n))...)
		if vs, err := viewblock.Decode(ref, least); err != nil || len(vs) != n {
			t.Fatalf("%d views in %d zero bytes: %v", n, len(least)-1, err)
		}
		if _, err := viewblock.Decode(ref, least[:len(least)-1]); err == nil || !strings.Contains(err.Error(), "fewer than") {
			t.Fatalf("%d views in one byte fewer: %v", n, err)
		}
	}
	ref := viewblock.Ref{FirstSeq: 1, Count: viewblock.MaxViews}
	payload := []byte{0, 0, 0, 0, 0}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := viewblock.Decode(ref, payload)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "fewer than") {
		t.Fatalf("MaxViews in 4 bytes: %v", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("refusing MaxViews in 4 bytes allocated %d bytes", grew)
	}
}
