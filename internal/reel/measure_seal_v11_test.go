//go:build linux || darwin

package reel

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end cast ZIP publication through the CLI on synthetic scratch only.
func TestSealMeasureExport(t *testing.T) {
	if os.Getenv("AREEL_SEAL_MEASURE") != "1" || os.Getenv("AREEL_LOOPOC_OUT") == "" {
		t.Skip("requires opt-in scratch stores")
	}
	root := filepath.Clean(os.Getenv("AREEL_LOOPOC_OUT"))
	var in struct {
		Dir    string `json:"dir"`
		Sealed string `json:"sealed"`
	}
	b, e := os.ReadFile(filepath.Join(root, "reader-mixed-8h.json"))
	if os.IsNotExist(e) {
		t.Skip("mixed-8h measurement unavailable")
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &in); e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(filepath.Clean(in.Dir), root+"/") || (in.Sealed != "" && !strings.HasPrefix(filepath.Clean(in.Sealed), root+"/")) {
		t.Fatal("source outside scratch")
	}
	work, e := os.MkdirTemp(root, "export-")
	if e != nil {
		t.Fatal(e)
	}
	id := filepath.Base(in.Dir)
	sealedRoot := filepath.Join(work, "sealed-library")
	if e = os.MkdirAll(filepath.Join(sealedRoot, "recordings"), 0700); e != nil {
		t.Fatal(e)
	}
	if in.Sealed != "" {
		src, e := os.Open(in.Sealed)
		if e != nil {
			t.Fatal(e)
		}
		dst, e := os.OpenFile(filepath.Join(sealedRoot, id+".areel"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		_, e = io.Copy(dst, src)
		if e != nil {
			t.Fatal(e)
		}
		if e = src.Close(); e != nil {
			t.Fatal(e)
		}
		if e = dst.Close(); e != nil {
			t.Fatal(e)
		}
	}
	type result struct {
		Form  string `json:"form"`
		Ns    int64  `json:"ns"`
		Bytes int64  `json:"bytes"`
		Load  string `json:"loadavg"`
		Error string `json:"error,omitempty"`
	}
	var results []result
	var castDigest []byte
	for _, form := range []string{"directory", "sealed"} {
		if form == "sealed" && in.Sealed == "" {
			results = append(results, result{Form: form, Load: "not run", Error: "seal could not be published within the verification limits"})
			continue
		}
		library := filepath.Dir(filepath.Dir(in.Dir))
		if form == "sealed" {
			library = sealedRoot
		}
		for n := 0; n < 3; n++ {
			out := filepath.Join(work, fmt.Sprintf("%s-%d.zip", form, n))
			r := result{Form: form, Load: "unavailable"}
			if raw, e := os.ReadFile("/proc/loadavg"); e == nil {
				r.Load = strings.TrimSpace(string(raw))
			}
			start := time.Now()
			e := CLI([]string{"export", id, out, "--library", library, "--format", "cast"}, io.Discard, io.Discard)
			r.Ns = int64(time.Since(start))
			if e != nil {
				r.Error = e.Error()
			} else {
				st, e := os.Stat(out)
				if e != nil {
					t.Fatal(e)
				}
				r.Bytes = st.Size()
				z, e := zip.OpenReader(out)
				if e != nil {
					t.Fatal(e)
				}
				hash := sha256.New()
				casts := 0
				for _, entry := range z.File {
					if !strings.HasSuffix(entry.Name, ".cast") {
						continue
					}
					casts++
					f, e := entry.Open()
					if e != nil {
						t.Fatal(e)
					}
					if _, e = io.Copy(hash, f); e != nil {
						t.Fatal(e)
					}
					if e = f.Close(); e != nil {
						t.Fatal(e)
					}
				}
				if e = z.Close(); e != nil {
					t.Fatal(e)
				}
				if casts == 0 {
					t.Fatal("export contains no cast")
				}
				if castDigest == nil {
					castDigest = hash.Sum(nil)
				} else if !bytes.Equal(castDigest, hash.Sum(nil)) {
					t.Fatal("cast payload differs between forms/repetitions")
				}

			}
			results = append(results, r)
			t.Logf("%s sample %d: %.3fs, %d bytes, error=%s", form, n+1, float64(r.Ns)/1e9, r.Bytes, r.Error)
		}
	}
	b, e = json.MarshalIndent(results, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "export-results.json"), append(b, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
}
