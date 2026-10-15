//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testLibraryAccess rebinds a test handler to its real listener address.
func testLibraryAccess(t testing.TB, listen string, config libraryAccessConfig) *libraryAccess {
	t.Helper()
	a, e := newLibraryAccess(listen, config)
	if e != nil {
		t.Fatal(e)
	}
	return a
}

const testTailscaleHost = "agent-reel.example.ts.net"

var testTailscaleConfig = libraryAccessConfig{AllowHosts: []string{"Agent-Reel.example.ts.net"}, TailscaleUsers: []string{"alice@github", "other@github"}, TailscaleSources: []string{"100.101.102.103", "fd7a:115c:a1e0::1"}}

// TestLibraryAccessHostPeerIdentity is the Host × peer × identity table over
// every route family: pages/API, export GET/POST (through the library and on
// the export handler itself) and the live websocket route.
func TestLibraryAccessHostPeerIdentity(t *testing.T) {
	h, s := libraryFixture(t)
	h.access = testLibraryAccess(t, libraryTestHost, testTailscaleConfig)
	exports, e := newLibraryExportsAccess(h.root, h.access)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = exports.Close() })
	exports.build = func(context.Context, string, exportSpec, string) (string, error) {
		return "", errors.New("synthetic build refused")
	}
	login := func(values ...string) func(*http.Request) {
		return func(r *http.Request) {
			for _, v := range values {
				r.Header.Add(libraryTailscaleUserHeader, v)
			}
		}
	}
	xff := func(values ...string) func(*http.Request) {
		return func(r *http.Request) {
			for _, v := range values {
				r.Header.Add("X-Forwarded-For", v)
			}
		}
	}
	type row struct {
		name, host, peer string
		header           func(*http.Request)
		admitted         bool
	}
	rows := []row{
		{"listen", libraryTestHost, "127.0.0.1:5", nil, true},
		{"listen-ipv6-loopback-peer", libraryTestHost, "[::1]:5", nil, true},
		{"localhost-alias", "localhost:43210", "127.0.0.1:5", nil, true},
		{"localhost-alias-case", "LocalHost:43210", "127.0.0.1:5", nil, true},
		{"allowed-host-login", testTailscaleHost, "127.0.0.1:5", login("alice@github"), true},
		{"allowed-host-second-login", testTailscaleHost, "127.0.0.1:5", login("other@github"), true},
		{"allowed-host-case", strings.ToUpper(testTailscaleHost), "127.0.0.1:5", login("alice@github"), true},
		{"allowed-host-missing-login", testTailscaleHost, "127.0.0.1:5", nil, false},
		{"allowed-host-empty-login", testTailscaleHost, "127.0.0.1:5", login(""), false},
		{"allowed-host-wrong-login", testTailscaleHost, "127.0.0.1:5", login("mallory@github"), false},
		{"allowed-host-login-case", testTailscaleHost, "127.0.0.1:5", login("JEPRECATED@github"), false},
		{"allowed-host-duplicate-login", testTailscaleHost, "127.0.0.1:5", login("alice@github", "alice@github"), false},
		{"allowed-host-mixed-logins", testTailscaleHost, "127.0.0.1:5", login("mallory@github", "alice@github"), false},
		{"allowed-host-other-port", testTailscaleHost + ":443", "127.0.0.1:5", login("alice@github"), false},
		{"allowed-host-remote-peer", testTailscaleHost, "100.64.0.9:5", login("alice@github"), false},
		{"listen-remote-peer", libraryTestHost, "192.0.2.1:5", nil, false},
		{"alias-remote-peer", "localhost:43210", "192.0.2.1:5", nil, false},
		{"unknown-host", "evil.example", "127.0.0.1:5", login("alice@github"), false},
		{"alias-other-port", "localhost:43211", "127.0.0.1:5", nil, false},
		{"listen-other-port", "127.0.0.1:43211", "127.0.0.1:5", nil, false},
		{"sibling-name", "agent-reel.example.ts.net.evil.example", "127.0.0.1:5", login("alice@github"), false},
		// Tailscale Serve forwards the client's Host unchanged: a tailnet
		// client claiming the listen Host still arrives with proxy headers.
		{"proxied-listen-host", libraryTestHost, "127.0.0.1:5", func(r *http.Request) { r.Header.Set("X-Forwarded-Host", libraryTestHost) }, false},
		{"proxied-alias-host", "localhost:43210", "127.0.0.1:5", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "100.64.0.9") }, false},
		{"listen-with-login", libraryTestHost, "127.0.0.1:5", login("alice@github"), false},
		// Tagged nodes carry no login: Serve's single X-Forwarded-For source.
		{"allowed-host-source", testTailscaleHost, "127.0.0.1:5", xff("100.101.102.103"), true},
		{"allowed-host-source-ipv6", testTailscaleHost, "127.0.0.1:5", xff("fd7a:115c:a1e0::1"), true},
		{"allowed-host-source-ipv4-mapped", testTailscaleHost, "127.0.0.1:5", xff("::ffff:100.101.102.103"), true},
		{"allowed-host-source-ipv6-noncanonical", testTailscaleHost, "127.0.0.1:5", xff("FD7A:115C:A1E0:0:0:0:0:1"), true},
		{"allowed-host-source-and-wrong-login", testTailscaleHost, "127.0.0.1:5", func(r *http.Request) { xff("100.101.102.103")(r); login("mallory@github")(r) }, true},
		{"allowed-host-other-source", testTailscaleHost, "127.0.0.1:5", xff("100.1.1.1"), false},
		{"allowed-host-source-list", testTailscaleHost, "127.0.0.1:5", xff("100.101.102.103, 100.101.102.103"), false},
		{"allowed-host-source-list-tight", testTailscaleHost, "127.0.0.1:5", xff("100.1.1.1,100.101.102.103"), false},
		{"allowed-host-source-two-headers", testTailscaleHost, "127.0.0.1:5", xff("100.101.102.103", "100.101.102.103"), false},
		{"allowed-host-source-malformed", testTailscaleHost, "127.0.0.1:5", xff("100.101.102.103x"), false},
		{"allowed-host-source-with-port", testTailscaleHost, "127.0.0.1:5", xff("100.101.102.103:443"), false},
		{"allowed-host-source-spaced", testTailscaleHost, "127.0.0.1:5", xff(" 100.101.102.103"), false},
		{"allowed-host-source-prefix", testTailscaleHost, "127.0.0.1:5", xff("100.101.102.103/32"), false},
		{"allowed-host-source-zone", testTailscaleHost, "127.0.0.1:5", xff("fd7a:115c:a1e0::1%eth0"), false},
		{"allowed-host-source-empty", testTailscaleHost, "127.0.0.1:5", xff(""), false},
		{"allowed-host-source-remote-peer", testTailscaleHost, "100.101.102.103:5", xff("100.101.102.103"), false},
		{"listen-host-source", libraryTestHost, "127.0.0.1:5", xff("100.101.102.103"), false},
		{"alias-host-source", "localhost:43210", "127.0.0.1:5", xff("100.101.102.103"), false},
		{"unknown-host-source", "evil.example", "127.0.0.1:5", xff("100.101.102.103"), false},
	}
	routes := []struct {
		name, method, path, body string
		direct                   bool
	}{
		{"page", "GET", "/", "", false},
		{"api", "GET", "/api/recordings/" + s.ID, "", false},
		{"export-form", "GET", "/exports/new?recording=" + s.ID, "", false},
		{"export-post", "POST", "/exports", "recording=" + s.ID + "&format=cast", false},
		{"export-post-direct", "POST", "/exports", "recording=" + s.ID + "&format=cast", true},
		{"export-cancel-direct", "POST", "/exports/" + strings.Repeat("b", 32) + "/cancel", "", true},
		{"live", "GET", "/api/recordings/" + s.ID + "/segments/1/live", "", false},
		{"download", "GET", "/download/" + s.ID, "", false},
	}
	for _, route := range routes {
		for _, tc := range rows {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				r := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				if route.body == "" {
					r.ContentLength = 0
				}
				if route.method == "POST" {
					r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				r.Host, r.RemoteAddr = tc.host, tc.peer
				if tc.header != nil {
					tc.header(r)
				}
				w := httptest.NewRecorder()
				if route.direct {
					exports.ServeHTTP(w, r)
				} else {
					h.ServeHTTP(w, r)
				}
				if tc.admitted && w.Code == http.StatusForbidden {
					t.Fatalf("refused admitted request: %d %s", w.Code, w.Body)
				}
				if !tc.admitted && w.Code != http.StatusForbidden {
					t.Fatalf("admitted refused request: %d %s", w.Code, w.Body)
				}
				if tc.admitted && (route.name == "page" || route.name == "api") && w.Code != http.StatusOK {
					t.Fatalf("admitted page not served: %d", w.Code)
				}
			})
		}
	}
	// Non-host refusals still apply on an allowed host with the right login.
	for name, change := range map[string]func(*http.Request){
		"range": func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") },
		"body":  func(r *http.Request) { r.ContentLength = 1; r.Body = io.NopCloser(strings.NewReader("x")) },
		"escape": func(r *http.Request) {
			r.URL.Path = "/api/recordings/" + s.ID + "/x"
			r.URL.RawPath = "/api/recordings/" + s.ID + "%2fx"
		},
		"chunked": func(r *http.Request) { r.TransferEncoding = []string{"chunked"} },
	} {
		w := libraryRequest(h, "/api/recordings/"+s.ID, func(r *http.Request) {
			r.Host = testTailscaleHost
			r.Header.Set(libraryTailscaleUserHeader, "alice@github")
			change(r)
		})
		if w.Code < 400 {
			t.Fatalf("%s admitted on allowed host: %d", name, w.Code)
		}
	}
}

// Without --allow-host, the Tailscale name is just an unknown Host.
func TestLibraryAccessDefaultRefusesProxyHosts(t *testing.T) {
	h, s := libraryFixture(t)
	w := libraryRequest(h, "/api/recordings/"+s.ID, func(r *http.Request) {
		r.Host = testTailscaleHost
		r.Header.Set(libraryTailscaleUserHeader, "alice@github")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("unconfigured proxy host admitted: %d", w.Code)
	}
	if w := libraryRequest(h, "/api/recordings/"+s.ID, func(r *http.Request) { r.Host = "localhost:43210" }); w.Code != http.StatusOK {
		t.Fatalf("localhost alias refused: %d", w.Code)
	}
}

func TestLibraryAccessConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		config libraryAccessConfig
		want   string
	}{
		{"host-without-user", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}}, "requires at least one --tailscale-user or --tailscale-source"},
		{"source-hostname", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"lattice"}}, "--tailscale-source"},
		{"source-prefix", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"100.64.0.0/10"}}, "--tailscale-source"},
		{"source-list", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"100.64.0.1,100.64.0.2"}}, "--tailscale-source"},
		{"source-empty", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{""}}, "--tailscale-source"},
		{"source-spaced", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{" 100.64.0.1"}}, "--tailscale-source"},
		{"source-port", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"100.64.0.1:443"}}, "--tailscale-source"},
		{"source-bracketed", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"[fd7a:115c:a1e0::1]"}}, "--tailscale-source"},
		{"source-zone", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"fe80::1%eth0"}}, "--tailscale-source"},
		{"source-mapped", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"::ffff:100.64.0.1"}}, "--tailscale-source"},
		{"source-unspecified", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"0.0.0.0"}}, "--tailscale-source"},
		{"scheme", libraryAccessConfig{AllowHosts: []string{"https://" + testTailscaleHost}, TailscaleUsers: []string{"a@github"}}, "without scheme or path"},
		{"path", libraryAccessConfig{AllowHosts: []string{testTailscaleHost + "/x"}, TailscaleUsers: []string{"a@github"}}, "without scheme or path"},
		{"bad-port", libraryAccessConfig{AllowHosts: []string{testTailscaleHost + ":0"}, TailscaleUsers: []string{"a@github"}}, "host:port"},
		{"empty-port", libraryAccessConfig{AllowHosts: []string{testTailscaleHost + ":"}, TailscaleUsers: []string{"a@github"}}, "host:port"},
		{"userinfo", libraryAccessConfig{AllowHosts: []string{"a@" + testTailscaleHost}, TailscaleUsers: []string{"a@github"}}, "without scheme"},
		{"empty-host", libraryAccessConfig{AllowHosts: []string{""}, TailscaleUsers: []string{"a@github"}}, "bare host name"},
		{"trailing-dot", libraryAccessConfig{AllowHosts: []string{testTailscaleHost + "."}, TailscaleUsers: []string{"a@github"}}, "bare host name"},
		{"ipv6", libraryAccessConfig{AllowHosts: []string{"[::1]:8765"}, TailscaleUsers: []string{"a@github"}}, "bare host name"},
		{"empty-user", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleUsers: []string{""}}, "--tailscale-user"},
		{"spaced-user", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleUsers: []string{" a@github"}}, "--tailscale-user"},
		{"list-user", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}, TailscaleUsers: []string{"a@github,b@github"}}, "--tailscale-user"},
	}
	for _, tc := range cases {
		if _, e := tc.config.validate(); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, e)
		}
	}
	for _, ok := range []libraryAccessConfig{{}, testTailscaleConfig, {AllowHosts: []string{"reel.example:8443"}, TailscaleUsers: []string{"a@github"}}, {TailscaleUsers: []string{"a@github"}}, {AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"100.64.0.1"}}, {AllowHosts: []string{testTailscaleHost}, TailscaleSources: []string{"fd7a:115c:a1e0::1"}}} {
		if _, e := ok.validate(); e != nil {
			t.Fatalf("valid config refused %+v: %v", ok, e)
		}
	}
	if _, e := newLibraryAccess("localhost:8765", libraryAccessConfig{}); e == nil {
		t.Fatal("non-literal listen accepted")
	}
}

func TestServiceUIAccessFlags(t *testing.T) {
	s := testStore(t)
	var got []libraryAccessConfig
	deps := openDeps{
		serve: func(string, string, io.Writer, func(string)) error { t.Fatal("unnamed serve"); return nil },
		serveNamed: func(_, _ string, _ io.Writer, _ func(string), _, _ string, access libraryAccessConfig) error {
			got = append(got, access)
			return nil
		},
		probe:  func(string) bool { return false },
		browse: func(string) error { t.Fatal("browser"); return nil },
	}
	args := []string{"ui", "--library", s.Root, "--allow-host", testTailscaleHost, "--tailscale-user", "a@github", "--tailscale-user", "b@github", "--allow-host", "other.example", "--tailscale-source", "100.64.0.1", "--tailscale-source", "fd7a:115c:a1e0::1"}
	if e := serviceUICommand(args, io.Discard, io.Discard, deps); e != nil {
		t.Fatal(e)
	}
	if len(got) != 1 || strings.Join(got[0].AllowHosts, " ") != testTailscaleHost+" other.example" || strings.Join(got[0].TailscaleUsers, " ") != "a@github b@github" || strings.Join(got[0].TailscaleSources, " ") != "100.64.0.1 fd7a:115c:a1e0::1" {
		t.Fatalf("flags not passed: %+v", got)
	}
	e := serviceUICommand([]string{"ui", "--library", s.Root, "--allow-host", testTailscaleHost}, io.Discard, io.Discard, deps)
	var usage *usageError
	if e == nil || !errors.As(e, &usage) || !strings.Contains(e.Error(), "requires at least one --tailscale-user or --tailscale-source") || len(got) != 1 {
		t.Fatalf("fail-open startup: %v", e)
	}
	// A source alone satisfies --allow-host; a malformed source is refused.
	if e := serviceUICommand([]string{"ui", "--library", s.Root, "--allow-host", testTailscaleHost, "--tailscale-source", "100.64.0.1"}, io.Discard, io.Discard, deps); e != nil || len(got) != 2 {
		t.Fatalf("source-only config refused: %v", e)
	}
	if e := serviceUICommand([]string{"ui", "--library", s.Root, "--allow-host", testTailscaleHost, "--tailscale-source", "100.64.0.0/10"}, io.Discard, io.Discard, deps); e == nil || !errors.As(e, &usage) || !strings.Contains(e.Error(), "--tailscale-source") || len(got) != 2 {
		t.Fatalf("prefix source accepted: %v", e)
	}
	if e := serviceUICommand([]string{"ui", "--library", s.Root, "--allow-host", "https://x.example/", "--tailscale-user", "a@github"}, io.Discard, io.Discard, deps); e == nil || len(got) != 2 {
		t.Fatalf("URL host accepted: %v", e)
	}
	var help bytes.Buffer
	if e := serviceUICommand([]string{"ui", "--help"}, &help, io.Discard, deps); e != nil || !strings.Contains(help.String(), "--allow-host HOST") || !strings.Contains(help.String(), "--tailscale-user LOGIN") || !strings.Contains(help.String(), "--tailscale-source IP") || !strings.Contains(help.String(), "tagged nodes") || !strings.Contains(help.String(), "Tailscale Serve") {
		t.Fatalf("help: %v %s", e, help.String())
	}
	// The production server refuses the same configuration before listening.
	if e := libraryServeNamed(s.Root, "127.0.0.1:0", io.Discard, nil, "{id}", "asciinema", libraryAccessConfig{AllowHosts: []string{testTailscaleHost}}); e == nil || !strings.Contains(e.Error(), "--tailscale-user") {
		t.Fatalf("server started fail-open: %v", e)
	}
}

// The page refusal of an allowed host names the identity problem; other
// refusals keep the generic text, and neither echoes request data.
func TestLibraryAccessRefusalPage(t *testing.T) {
	h, _ := libraryFixture(t)
	h.access = testLibraryAccess(t, libraryTestHost, testTailscaleConfig)
	for _, tc := range []struct {
		name, host string
		header     func(*http.Request)
		identity   bool
	}{
		{"allowed-host-no-identity", testTailscaleHost, nil, true},
		{"allowed-host-wrong-source", testTailscaleHost, func(r *http.Request) { r.Header.Set("X-Forwarded-For", "100.1.1.1") }, true},
		{"allowed-host-range", testTailscaleHost, func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "100.101.102.103")
			r.Header.Set("Range", "bytes=0-1")
		}, false},
		{"unknown-host", "evil.example", nil, false},
		{"proxied-listen", libraryTestHost, func(r *http.Request) { r.Header.Set("X-Forwarded-For", "100.101.102.103") }, false},
	} {
		w := libraryRequest(h, "/", func(r *http.Request) {
			r.Host = tc.host
			if tc.header != nil {
				tc.header(r)
			}
		})
		body := w.Body.String()
		if w.Code != http.StatusForbidden || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Header().Get("Content-Type"))
		}
		if got := strings.Contains(body, "Tailscale didn&#39;t identify a permitted login or device"); got != tc.identity {
			t.Fatalf("%s: identity text %v: %s", tc.name, got, body)
		}
		if strings.Contains(body, tc.host) || strings.Contains(body, "100.") {
			t.Fatalf("%s: refusal echoes request data: %s", tc.name, body)
		}
	}
}
