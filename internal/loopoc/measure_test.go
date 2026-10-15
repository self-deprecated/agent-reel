package loopoc

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"testing"
)

var synthetic = SyntheticCorpora()

func TestSyntheticCorporaValid(t *testing.T) {
	for _, c := range synthetic {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		// Production commit rule: no batch above BatchRecords views.
		last := int64(0)
		for _, b := range c.Commits {
			if b-last > BatchRecords {
				t.Fatalf("%s: batch of %d views", c.Name, b-last)
			}
			last = b
		}
		t.Logf("%-14s views=%-5d commits=%-4d screens=%d", c.Name, len(c.Views), len(c.Commits), len(Screens(c.Views)))
	}
}

// TestGeneratorsDeterministic: the same seed gives byte-identical casts and an
// identical views hash; another seed changes a jittered workload.
func TestGeneratorsDeterministic(t *testing.T) {
	for _, name := range V9Workloads {
		a, errA := Workload(name, 7, SyntheticSpan(name))
		b, errB := Workload(name, 7, SyntheticSpan(name))
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		var ca, cb bytes.Buffer
		if err := a.WriteCast(&ca); err != nil {
			t.Fatal(err)
		}
		if err := b.WriteCast(&cb); err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(ca.Bytes()) != sha256.Sum256(cb.Bytes()) {
			t.Fatalf("%s: casts differ for one seed", name)
		}
		if ViewsHash(Synthesize(a).Views) != ViewsHash(Synthesize(b).Views) {
			t.Fatalf("%s: views differ for one seed", name)
		}
		if name == S3Jitter {
			c, _ := Workload(name, 8, SyntheticSpan(name))
			if ViewsHash(Synthesize(c).Views) == ViewsHash(Synthesize(a).Views) {
				t.Fatalf("%s: seed does not matter", name)
			}
		}
	}
}

func syntheticNamed(t *testing.T, name string) (Corpus, Script) {
	t.Helper()
	s, err := Workload(name, SyntheticSeed, SyntheticSpan(name))
	if err != nil {
		t.Fatal(err)
	}
	return Synthesize(s), s
}

func TestSyntheticClockUnmasked(t *testing.T) {
	c, s := syntheticNamed(t, S2Clock)
	assertClockUnmasked(t, c, s)
}

func TestSyntheticShapes(t *testing.T) {
	// S4 changes geometry twice, so its screens split into three sets.
	_, s4 := syntheticNamed(t, S4Resize)
	resizes := 0
	for _, st := range s4.Steps {
		if st.Cols != 0 {
			resizes++
		}
	}
	if resizes != 2 {
		t.Fatalf("S4 has %d resizes", resizes)
	}
	// S5 reaches every break kind.
	_, s5 := syntheticNamed(t, S5Diverge)
	var breaks []string
	for _, st := range s5.Steps {
		if len(st.Note) > 6 && st.Note[:6] == "break:" {
			breaks = append(breaks, st.Note[6:])
		}
	}
	want := []string{"skip", "reverse", "oneoff", "text", "period", "spinner"}
	if !slices.Equal(breaks, want) {
		t.Fatalf("S5 breaks %v, want %v", breaks, want)
	}
	// S1 is a pure 10-frame loop after the status/header views.
	s1, _ := syntheticNamed(t, S1Pure)
	for i := 12; i < len(s1.Views); i++ {
		if s1.Views[i].ScreenID != s1.Views[i-10].ScreenID || s1.Views[i].AtNs-s1.Views[i-1].AtNs != nominal {
			t.Fatalf("S1 not a pure loop at seq %d", s1.Views[i].Seq)
		}
	}
}

func TestPrefixes(t *testing.T) {
	c, _ := syntheticNamed(t, S3Jitter)
	ps := Prefixes(c)
	crashes, emptyCrash := 0, false
	for _, p := range ps {
		n := int64(len(p.Views))
		if n > 0 && p.Commits[len(p.Commits)-1] != n {
			t.Fatalf("%s: last commit %d, %d views", p.Name, p.Commits[len(p.Commits)-1], n)
		}
		if !slices.Contains(c.Commits, n) && n != 0 {
			t.Fatalf("%s: truncated at non-boundary %d", p.Name, n)
		}
		if len(p.Tail) > 0 {
			crashes++
			if p.Tail[0].Seq != n+1 {
				t.Fatalf("%s: tail starts at %d", p.Name, p.Tail[0].Seq)
			}
			emptyCrash = emptyCrash || n == 0
		}
	}
	if len(ps)-crashes != len(c.Commits) || crashes < 16 || !emptyCrash {
		t.Fatalf("%d prefixes, %d crashes (empty crash %v), %d commits", len(ps), crashes, emptyCrash, len(c.Commits))
	}
}

func TestBaselinesExact(t *testing.T) {
	for _, codec := range []Codec{Raw{}, SQLiteV8{Dir: t.TempDir()}} {
		for _, c := range synthetic {
			for _, policy := range Policies {
				r := MeasurePolicy(c, codec, policy)
				if !r.Exact || r.PrefixesChecked != len(Prefixes(c)) {
					t.Fatalf("%s %s %s: exact=%v prefixes=%d/%d seq %d %s: %s %s", r.Codec, c.Name, policy, r.Exact, r.PrefixesChecked, len(Prefixes(c)), r.FirstBadSeq, r.BadCheck, r.BadDetail, r.Err)
				}
				if codec.Name() == "raw" && policy == PolicyIdeal && r.StoredBytes != RawViewBytes*int64(len(c.Views))+RecordOverhead {
					t.Fatalf("raw ideal bytes %d", r.StoredBytes)
				}
				t.Logf("%-9s %-14s %-5s B/view=%6.2f B/h=%9.0f seek p50=%dns phase=%d exact=%v prefixes=%d", r.Codec, r.Corpus, r.Policy, r.BytesPerView, r.BytesPerHour, r.SeekP50Ns, r.PhaseSeeks, r.Exact, r.PrefixesChecked)
			}
		}
	}
}

// broken wraps a codec and perturbs its decoder's output.
type broken struct {
	Codec
	name    string
	perturb func(*View)
}

func (b broken) Name() string { return b.name }

func (b broken) Open(records [][]byte, screens ScreenTable) (Decoder, error) {
	d, err := b.Codec.Open(records, screens)
	if err != nil {
		return nil, err
	}
	return brokenDecoder{d, b.perturb}, nil
}

type brokenDecoder struct {
	Decoder
	perturb func(*View)
}

func (d brokenDecoder) At(seq int64) (View, error) {
	v, err := d.Decoder.At(seq)
	d.perturb(&v)
	return v, err
}

func (d brokenDecoder) Range(from int64, n int) ([]View, error) {
	vs, err := d.Decoder.Range(from, n)
	for i := range vs {
		d.perturb(&vs[i])
	}
	return vs, err
}

// TestMeasureDetectsCorruption: an AtNs off by 1 ns and a dropped chain both
// make Exact false at the right seq.
func TestMeasureDetectsCorruption(t *testing.T) {
	c, _ := syntheticNamed(t, S3Jitter)
	const target = 137
	cases := []struct {
		name    string
		perturb func(*View)
	}{
		{"at-plus-1ns", func(v *View) {
			if v.Seq == target {
				v.AtNs++
			}
		}},
		{"drop-chain", func(v *View) {
			if v.Seq == target {
				v.Chain = [32]byte{}
			}
		}},
	}
	for _, tc := range cases {
		for _, policy := range Policies {
			r := MeasurePolicy(c, broken{Raw{}, tc.name, tc.perturb}, policy)
			if r.Exact || r.FirstBadSeq != target {
				t.Fatalf("%s/%s: exact=%v first bad seq %d (%s: %s), want seq %d", tc.name, policy, r.Exact, r.FirstBadSeq, r.BadCheck, r.BadDetail, target)
			}
			t.Logf("%s/%s: detected at seq %d by %s: %s", tc.name, policy, r.FirstBadSeq, r.BadCheck, r.BadDetail)
		}
	}
}
