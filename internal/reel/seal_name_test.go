package reel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSealName(t *testing.T) {
	id := strings.Repeat("a", 32)
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	started := time.Date(2026, 9, 24, 0, 30, 12, 0, time.FixedZone("east", 2*3600))
	state := State{ID: id, Target: Target{Backend: "fake", Name: "spaces / slashes-é"}, created: created, Segments: []Segment{{Started: started}}}
	template := "{date}-{time}-{backend}-{target}-{id}"
	got, err := renderSealName(template, state)
	want := "2026-09-23-223012-fake-spaces___slashes-__-" + id
	if err != nil || got != want {
		t.Fatalf("first segment UTC and byte mapping: %q %v; want %q", got, err, want)
	}
	re, err := sealNameRegexp(template)
	if err != nil || len(re.FindStringSubmatch(got+".areel")) != 2 || re.FindStringSubmatch(got + ".areel")[1] != id {
		t.Fatalf("regexp: %v %v", re, err)
	}
	state.Segments = nil
	if got, err = renderSealName("{date}-{time}-{id}", state); err != nil || got != "2025-01-02-030405-"+id {
		t.Fatalf("created fallback: %q %v", got, err)
	}
	state.Target.Name = strings.Repeat("x", 80)
	if got, err = renderSealName("{target}-{id}", state); err != nil || got != strings.Repeat("x", 64)+"-"+id {
		t.Fatalf("target cap: %q %v", got, err)
	}
	if _, err = renderSealName(strings.Repeat("x", 120)+"{target}{id}", state); err == nil || !strings.Contains(err.Error(), "200 bytes") {
		t.Fatalf("actual rendering bound: %v", err)
	}
	for _, template := range []string{"{id}", "x.{id}", "{date}-{target}-{id}"} {
		re, err := sealNameRegexp(template)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"config.json", "lock", "recordings", "targets", "." + id + ".areel.tmp-0123456789abcdef", "." + id + ".areel.salvage-0123456789abcdef", "." + id + ".deleting-0123456789abcdef"} {
			if re.MatchString(name) {
				t.Fatalf("template %q matches nonpublished root entry %q", template, name)
			}
		}
	}
}

func TestSealNameSettings(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.json")
	env := map[string]string{"AREEL_CONFIG": file}
	getenv := func(k string) string { return env[k] }
	for _, tc := range []struct{ template, rule string }{
		{"prefix", "exactly one {id}"}, {"{id}-{id}", "exactly one {id}"},
		{".{id}", "must not start with ."}, {"x..{id}", "must not contain .."},
		{"x/{id}", "literal bytes"}, {"{wat}-{id}", "unknown token"},
		{strings.Repeat("x", 169) + "{id}", "200 bytes"},
	} {
		for _, source := range []string{"file", "env"} {
			if source == "file" {
				writeUserConfig(t, file, map[string]any{"seal.name": tc.template})
				delete(env, "AREEL_SEAL_NAME")
			} else {
				writeUserConfig(t, file, map[string]any{})
				env["AREEL_SEAL_NAME"] = tc.template
			}
			if _, err := loadSettings(nil, getenv); err == nil || !strings.Contains(err.Error(), tc.rule) || !strings.Contains(err.Error(), "seal.name") {
				t.Fatalf("%s %q: %v; want %q", source, tc.template, err, tc.rule)
			}
		}
	}
	writeUserConfig(t, file, map[string]any{"seal.name": "{date}-{target}-{id}"})
	delete(env, "AREEL_SEAL_NAME")
	s, err := loadSettings(nil, getenv)
	if err != nil || s.SealName != "{date}-{target}-{id}" || s.source("seal.name") != "file "+file {
		t.Fatalf("file setting: %+v %v", s, err)
	}
	t.Setenv("AREEL_CONFIG", file)
	t.Setenv("AREEL_SEAL_NAME", "{backend}-{id}")
	out, _, err := cliRun(t, "config", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var view settingsView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range view.Settings {
		if item.Key == "seal.name" {
			found = item.Value == "{backend}-{id}" && item.Source == "env AREEL_SEAL_NAME"
		}
	}
	if !found {
		t.Fatalf("config lacks resolved seal.name: %s", out)
	}
}

func TestSealNameHomeManager(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "nix", "home-manager.nix"))
	if os.IsNotExist(err) {
		t.Skip("repo nix/home-manager.nix is absent from the Nix package source")
	}
	if err != nil {
		t.Fatal(err)
	}
	// Text-level check only: Nix evaluation is held. settingsJSON strips nulls
	// recursively from the entire settings set, so the new seal section remains.
	for _, want := range []string{"seal.name = setting sealName", "settingsJSON = strip (s", "lib.mapAttrs (_: strip) v", "builtins.toJSON settingsJSON", `lib.splitString "{id}" v`, "builtins.stringLength minimum <= 200"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("module missing %q", want)
		}
	}
}
