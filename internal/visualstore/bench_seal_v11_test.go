package visualstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// BenchmarkSealReaders measures existing Run 6 scratch stores. Use -cpu 1,
// -benchtime=1x -count=3: each iteration is 128 seeks/reads or one whole walk.
// Cold means a new application/SQLite connection, not a flushed OS page cache.
func BenchmarkSealReaders(b *testing.B) {
	if os.Getenv("AREEL_SEAL_MEASURE") != "1" || os.Getenv("AREEL_LOOPOC_OUT") == "" {
		b.Skip("requires opt-in scratch stores")
	}
	root := filepath.Clean(os.Getenv("AREEL_LOOPOC_OUT"))
	paths, e := filepath.Glob(filepath.Join(root, "reader-*.json"))
	if e != nil {
		b.Fatal(e)
	}
	if len(paths) == 0 {
		b.Skip("capture and seal stages have not produced stores")
	}
	for _, p := range paths {
		var in struct {
			Name   string `json:"name"`
			Dir    string `json:"dir"`
			Sealed string `json:"sealed"`
			Span   int64  `json:"span_ns"`
			Views  int64  `json:"views"`
		}
		data, e := os.ReadFile(p)
		if e != nil {
			b.Fatal(e)
		}
		if e = json.Unmarshal(data, &in); e != nil {
			b.Fatal(e)
		}
		if only := os.Getenv("AREEL_SEAL_ONLY"); only != "" && !slices.Contains(strings.Split(only, ","), in.Name) {
			continue
		}
		if !strings.HasPrefix(filepath.Clean(in.Dir), root+"/") || (in.Sealed != "" && !strings.HasPrefix(filepath.Clean(in.Sealed), root+"/")) {
			b.Fatal("store outside scratch")
		}
		for _, form := range []string{"directory", "sealed"} {
			if form == "sealed" && in.Sealed == "" {
				b.Run(in.Name+"/sealed", func(b *testing.B) { b.Skip("seal was not published; see the recorded verification limit failure") })
				continue
			}
			open := func() *visualstore.Reader {
				var r *visualstore.Reader
				var e error
				if form == "sealed" {
					r, e = visualstore.OpenSealed(in.Sealed)
				} else {
					r, e = visualstore.OpenReader(filepath.Join(in.Dir, "visual"))
				}
				if e != nil {
					b.Fatal(e)
				}
				return r
			}
			for _, op := range []string{"ViewAtCold", "ViewAtWarm", "ScreenRandom", "Views4096"} {
				b.Run(in.Name+"/"+form+"/"+op, func(b *testing.B) {
					ctx := context.Background()
					r := open()
					defer r.Close()
					sel, e := r.Snapshot(ctx)
					if e != nil {
						b.Fatal(e)
					}
					const queries = 128
					var sites [queries]visualtime.Time
					var screens [queries]int64
					// Fixed integer LCG: same sites and screen IDs in both forms and repetitions.
					state := uint64(0x56390009)
					for i := range sites {
						state = state*6364136223846793005 + 1
						sites[i] = visualtime.Time((state >> 1) % uint64(in.Span))
						v, e := r.ViewAt(ctx, sel, 1, sites[i])
						if e != nil {
							b.Fatal(e)
						}
						screens[i] = v.ScreenID
					}
					load := "unavailable"
					if raw, e := os.ReadFile("/proc/loadavg"); e == nil {
						load = strings.TrimSpace(string(raw))
					}
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						switch op {
						case "Views4096":
							var got int64
							for from := int64(1); ; {
								page, e := r.Views(ctx, sel, 1, from, 4096)
								if e != nil {
									b.Fatal(e)
								}
								if len(page) == 0 {
									break
								}
								got += int64(len(page))
								from = page[len(page)-1].Seq + 1
							}
							if got != in.Views {
								b.Fatalf("walked %d of %d", got, in.Views)
							}
						case "ViewAtCold":
							for _, at := range sites {
								rr := open()
								ss, e := rr.Snapshot(ctx)
								if e != nil {
									b.Fatal(e)
								}
								_, e = rr.ViewAt(ctx, ss, 1, at)
								if e != nil {
									b.Fatal(e)
								}
								if e = rr.Close(); e != nil {
									b.Fatal(e)
								}
							}
						case "ViewAtWarm":
							for _, at := range sites {
								b.StopTimer()
								_, e := r.ViewAt(ctx, sel, 1, at)
								if e != nil {
									b.Fatal(e)
								}
								b.StartTimer()
								if _, e = r.ViewAt(ctx, sel, 1, at); e != nil {
									b.Fatal(e)
								}
							}
						case "ScreenRandom":
							for _, id := range screens {
								if _, e := r.Screen(ctx, sel, id); e != nil {
									b.Fatal(e)
								}
							}
						}
					}
					b.StopTimer()
					ops := queries
					if op == "Views4096" {
						ops = 1
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*ops), "ns/query")
					result := map[string]any{"name": in.Name, "form": form, "operation": op, "ns": b.Elapsed().Nanoseconds(), "iterations": b.N, "queries_per_iteration": ops, "loadavg": load, "at": time.Now().UTC().Format(time.RFC3339Nano)}
					data, e := json.MarshalIndent(result, "", "  ")
					if e != nil {
						b.Fatal(e)
					}
					f, e := os.CreateTemp(root, fmt.Sprintf("bench-%s-%s-%s-*.json", in.Name, form, op))
					if e != nil {
						b.Fatal(e)
					}
					if _, e = f.Write(data); e != nil {
						b.Fatal(e)
					}
					if e = f.Close(); e != nil {
						b.Fatal(e)
					}
				})
			}
		}
	}
}
