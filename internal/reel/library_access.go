package reel

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// libraryAccessConfig is the optional reverse-proxy admission of the library
// UI: extra Host header values reached through a loopback proxy (Tailscale
// Serve), the Tailscale logins that proxy must vouch for, and the tailnet
// source addresses (tagged nodes, which carry no login) it may forward for.
type libraryAccessConfig struct {
	AllowHosts       []string
	TailscaleUsers   []string
	TailscaleSources []string
}

// libraryTailscaleUserHeader is set by Tailscale Serve for requests from
// user-owned tailnet devices; Serve strips client-supplied copies.
const libraryTailscaleUserHeader = "Tailscale-User-Login"

// libraryForwardedForHeader is overwritten by Tailscale Serve with the single
// tailnet source address of the connection; a client-supplied value never
// survives the proxy.
const libraryForwardedForHeader = "X-Forwarded-For"

// libraryProxyHeaders mark a request that crossed a reverse proxy. Tailscale
// Serve forwards the client's own Host header unchanged and always sets
// X-Forwarded-Host, so a proxied request is never admitted on the listen-host
// path, whatever Host it claims.
var libraryProxyHeaders = []string{"X-Forwarded-Host", "X-Forwarded-For", "X-Forwarded-Proto", "Forwarded", libraryTailscaleUserHeader, "Tailscale-User-Name", "Tailscale-User-Profile-Pic", "Tailscale-Headers-Info", "Tailscale-Funnel-Request", "Tailscale-App-Capabilities"}

// libraryAccess is the one Host/peer/identity admission shared by every
// library route: pages, API, exports and the live websocket proxy.
type libraryAccess struct {
	listen, alias string
	allowHosts    map[string]bool
	users         map[string]bool
	sources       map[netip.Addr]bool
}

// libraryAccessRules is a validated libraryAccessConfig.
type libraryAccessRules struct {
	hosts, users map[string]bool
	sources      map[netip.Addr]bool
}

// validate checks the configuration without a listen address. Allowing an
// extra Host without a Tailscale login or source address to require would
// admit any tailnet device (or tagged node) the proxy forwards, so it is
// refused.
func (c libraryAccessConfig) validate() (libraryAccessRules, error) {
	rules := libraryAccessRules{hosts: map[string]bool{}, users: map[string]bool{}, sources: map[netip.Addr]bool{}}
	for _, v := range c.AllowHosts {
		h, e := libraryAllowHost(v)
		if e != nil {
			return libraryAccessRules{}, e
		}
		rules.hosts[h] = true
	}
	for _, v := range c.TailscaleUsers {
		if v == "" || v != strings.TrimSpace(v) || strings.ContainsAny(v, " \t\r\n,") {
			return libraryAccessRules{}, fmt.Errorf("--tailscale-user %q: expected one Tailscale login such as user@github", v)
		}
		rules.users[v] = true
	}
	for _, v := range c.TailscaleSources {
		ip, e := netip.ParseAddr(v)
		if e != nil || ip.Zone() != "" || ip.Is4In6() || !ip.IsValid() || ip.IsUnspecified() {
			return libraryAccessRules{}, fmt.Errorf("--tailscale-source %q: expected one tailnet IP address such as 100.64.0.1 or fd7a:115c:a1e0::1 (no prefix, zone, port or IPv4-mapped form)", v)
		}
		rules.sources[ip] = true
	}
	if len(rules.hosts) > 0 && len(rules.users) == 0 && len(rules.sources) == 0 {
		return libraryAccessRules{}, fmt.Errorf("--allow-host requires at least one --tailscale-user or --tailscale-source: an extra Host is admitted only for a request that Tailscale Serve attributes to an allowed login (Tailscale-User-Login) or an allowed tailnet source address (X-Forwarded-For), otherwise every device that can reach the proxy would be let in")
	}
	return rules, nil
}

// libraryAllowHost canonicalizes one --allow-host value: a bare DNS name or
// an explicit name:port, lower-cased; never a URL, path, IP literal with
// brackets or user info.
func libraryAllowHost(v string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("--allow-host %q: expected a bare host name such as agent-reel.example.ts.net (or host:port), without scheme or path", v)
	}
	h := strings.ToLower(v)
	if h == "" || strings.ContainsAny(h, "/@?#[]% \t\r\n\\") {
		return bad()
	}
	name, port := h, ""
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		name, port = h[:i], h[i+1:]
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return bad()
		}
	}
	if name == "" || len(name) > 253 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return bad()
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return bad()
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return bad()
			}
		}
	}
	return h, nil
}

// newLibraryAccess binds the admission to the literal loopback listen
// address; localhost:PORT is its alias (an ssh -L tunnel or a browser typing
// localhost), with the same loopback-peer requirement.
func newLibraryAccess(listen string, c libraryAccessConfig) (*libraryAccess, error) {
	ip, port, e := net.SplitHostPort(listen)
	n, pe := strconv.Atoi(port)
	if e != nil || pe != nil || ip != "127.0.0.1" || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, fmt.Errorf("literal loopback host required")
	}
	rules, e := c.validate()
	if e != nil {
		return nil, e
	}
	return &libraryAccess{listen: listen, alias: "localhost:" + port, allowHosts: rules.hosts, users: rules.users, sources: rules.sources}, nil
}

// allowed admits a request only from a loopback peer and either
//   - to the listen address (or its localhost alias) with no proxy headers, or
//   - to an --allow-host name with exactly one Tailscale-User-Login naming a
//     configured login, or with exactly one single-address X-Forwarded-For
//     naming a configured --tailscale-source (tagged nodes, such as the Serve
//     host itself, carry no login).
//
// Those headers are trusted only because the peer is loopback: the local
// Tailscale Serve proxy is the only non-local path to this listener, it
// deletes client-supplied identity headers and overwrites X-Forwarded-For
// with the connection's tailnet source address.
func (a *libraryAccess) allowed(r *http.Request) bool {
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil || net.ParseIP(ip) == nil || !net.ParseIP(ip).IsLoopback() {
		return false
	}
	host := strings.ToLower(r.Host)
	if host == a.listen || host == a.alias {
		for _, name := range libraryProxyHeaders {
			if _, present := r.Header[http.CanonicalHeaderKey(name)]; present {
				return false
			}
		}
		return true
	}
	if !a.allowHosts[host] {
		return false
	}
	if logins := r.Header.Values(libraryTailscaleUserHeader); len(logins) == 1 && a.users[logins[0]] {
		return true
	}
	return a.sources[libraryForwardedSource(r)]
}

// identityRefused reports whether a refused request was addressed to an
// --allow-host name, so the refusal is about who Tailscale says is asking.
func (a *libraryAccess) identityRefused(r *http.Request) bool {
	return a.allowHosts[strings.ToLower(r.Host)]
}

// libraryForwardedSource is the one address of a single X-Forwarded-For
// header, unmapped; the zero Addr for anything else (absent, repeated,
// a list, a zone or not an IP).
func libraryForwardedSource(r *http.Request) netip.Addr {
	values := r.Header.Values(libraryForwardedForHeader)
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return netip.Addr{}
	}
	ip, e := netip.ParseAddr(values[0])
	if e != nil || ip.Zone() != "" {
		return netip.Addr{}
	}
	return ip.Unmap()
}
