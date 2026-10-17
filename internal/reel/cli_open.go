package reel

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

// openDeps are the process-level bindings of `areel open` and `areel service
// ui`; tests inject fakes so no server, probe or browser is involved.
type openDeps struct {
	serve      func(root, listen string, out io.Writer, ready func(string)) error
	serveNamed func(root, listen string, out io.Writer, ready func(string), name, theme string, access libraryAccessConfig) error
	probe      func(listen string) bool
	browse     func(url string) error
}

func openProduction() openDeps {
	return openDeps{serve: libraryServe, serveNamed: libraryServeNamed, probe: probeLibraryUI, browse: openBrowser}
}

var openHelp = cliCommand{Name: "open",
	Summary: `Browse the library in the local web UI (loopback only). If an areel library
UI already answers at ui.listen, print its URL; otherwise serve the UI in the
foreground at ui.listen (default 127.0.0.1:0, a free port) until Ctrl-C. A
browser opens only with --browser or ui.openBrowser = true.`,
	Examples: []string{"areel open", "areel open --browser", "areel open --listen 127.0.0.1:8765"},
	Docs:     "docs/library.md"}

var serviceUIHelp = cliCommand{Name: "service ui",
	Summary: `Integration: serve the library UI at ui.listen for a user service unit.
Never opens a browser. Prints "AgentReel local library: URL" once listening.

Requests must come from a loopback peer with Host set to the listen address
or localhost:PORT. To publish the UI through Tailscale Serve, name the Serve
hostname with --allow-host and the tailnet logins allowed to use it with
--tailscale-user; a request for an allowed host is served only when its single
Tailscale-User-Login header names one of those logins. Tagged nodes (such as
the Serve host itself) carry no login: name their tailnet IPs with
--tailscale-source, and a request whose single X-Forwarded-For address equals
one of them is served too. Only list tailnet IPs of devices you control.
--allow-host without --tailscale-user or --tailscale-source is refused. These
headers are trusted only because the peer is loopback: Serve deletes
client-supplied identity headers and overwrites X-Forwarded-For with the real
tailnet source address, but any local process can reach the listener and set
them, exactly as it can already use the UI directly.`,
	Examples: []string{"areel service ui", "areel service ui --listen 127.0.0.1:8765",
		"areel service ui --listen 127.0.0.1:8765 --allow-host agent-reel.example.ts.net --tailscale-user alice@github"},
	Docs: "docs/library.md"}

func openCLI(args []string, out, errOut io.Writer) error {
	return openCommand(args, out, errOut, openProduction())
}

func openCommand(args []string, out, errOut io.Writer, deps openDeps) error {
	f := newCLIFlags(openHelp, out)
	f.library()
	f.setting("listen", "ui.listen", "ADDR", "loopback address 127.0.0.1:PORT (default: ui.listen)")
	f.settingBool("browser", "ui.openBrowser", "open the URL in a browser (default: ui.openBrowser)")
	f.setting("theme", "ui.theme", "THEME", "default terminal colours, e.g. dracula or asciinema,bg=#5b2a86 (default: ui.theme, asciinema)")
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	s, root, e := uiLibrary(f, requireLibrary)
	if e != nil {
		return e
	}
	browse := func(url string) {
		if !s.UIOpenBrowser {
			return
		}
		if e := deps.browse(url); e != nil {
			fmt.Fprintf(errOut, "warning: could not open a browser: %v\n", e)
		}
	}
	if _, port, _ := net.SplitHostPort(s.UIListen); port != "0" && deps.probe(s.UIListen) {
		url := "http://" + s.UIListen + "/"
		if _, e = fmt.Fprintf(out, "AgentReel local library: %s (already running)\n", url); e != nil {
			return e
		}
		browse(url)
		return nil
	}
	if deps.serveNamed != nil {
		return deps.serveNamed(root, s.UIListen, out, browse, s.SealName, s.UITheme, libraryAccessConfig{})
	}
	return deps.serve(root, s.UIListen, out, browse)
}

func serviceUICLI(args []string, out, errOut io.Writer) error {
	return serviceUICommand(args, out, errOut, openProduction())
}

func serviceUICommand(args []string, out, errOut io.Writer, deps openDeps) error {
	f := newCLIFlags(serviceUIHelp, out)
	f.library()
	f.setting("listen", "ui.listen", "ADDR", "loopback address 127.0.0.1:PORT (default: ui.listen)")
	f.setting("theme", "ui.theme", "THEME", "default terminal colours, e.g. dracula or asciinema,bg=#5b2a86 (default: ui.theme, asciinema)")
	allowHosts := f.Strings("allow-host", "HOST", "also accept this Host header (host or host:port, e.g. the Tailscale Serve name); repeatable; requires --tailscale-user or --tailscale-source")
	users := f.Strings("tailscale-user", "LOGIN", "Tailscale login (Tailscale-User-Login) allowed on --allow-host hosts; repeatable")
	sources := f.Strings("tailscale-source", "IP", "tailnet IP (X-Forwarded-For set by Tailscale Serve) allowed on --allow-host hosts, for tagged nodes without a login; repeatable")
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	access := libraryAccessConfig{AllowHosts: *allowHosts, TailscaleUsers: *users, TailscaleSources: *sources}
	if _, e := access.validate(); e != nil {
		return usageErr("%s: %v", f.cmd.Name, e)
	}
	s, root, e := uiLibrary(f, serviceLibrary)
	if e != nil {
		return e
	}
	if deps.serveNamed != nil {
		return deps.serveNamed(root, s.UIListen, out, nil, s.SealName, s.UITheme, access)
	}
	if len(access.AllowHosts) > 0 {
		return fmt.Errorf("%s: this build cannot serve --allow-host", f.cmd.Name)
	}
	return deps.serve(root, s.UIListen, out, nil)
}

func uiLibrary(f *cliFlags, require func(*settings) (string, error)) (*settings, string, error) {
	s, e := f.load(nil)
	if e != nil {
		return nil, "", e
	}
	root, e := require(s)
	if e != nil {
		return nil, "", e
	}
	selected, e := libraryRoot(root)
	if e != nil {
		return nil, "", fmt.Errorf("library %s unavailable: an existing private (0700) directory is required: %w", root, e)
	}
	return s, selected, nil
}

// probeLibraryUI reports whether an areel library UI answers at listen: a
// loopback GET / whose HTML title names AgentReel.
func probeLibraryUI(listen string) bool {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	resp, e := client.Get("http://" + listen + "/")
	if e != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return bytes.Contains(body, []byte("<title>AgentReel"))
}

// openBrowser hands the URL to the desktop's opener without waiting for it.
func openBrowser(url string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "/usr/bin/open"
	}
	cmd := exec.Command(opener, url)
	if e := cmd.Start(); e != nil {
		return e
	}
	go func() { _ = cmd.Wait() }() // reap the opener; it usually exits at once
	return nil
}
