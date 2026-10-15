//go:build linux

package reel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

const liveTCPHeader = "  sl  local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"

func liveTCPRow(port int, inode int) string {
	return fmt.Sprintf("0: %08X:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 %d 1\n", binary.NativeEndian.Uint32([]byte{127, 0, 0, 1}), port, inode)
}

func liveSocketFixture() (socketProc, ProcessIdentity, map[string]string, map[string]string) {
	id := ProcessIdentity{PID: 99, Start: "1234", Boot: offlineView.Boot, PIDNamespace: offlineView.Namespace}
	files := map[string]string{
		"/proc/self/stat": "42 (test) R", "/proc/self/status": "NSpid:\t42\n",
		"/proc/self/mountinfo":            "1 0 0:1 / /proc rw - proc proc rw\n",
		"/proc/sys/kernel/random/boot_id": id.Boot,
		"/proc/99/stat":                   "99 (asciinema (test)) S " + strings.Repeat("0 ", 18) + id.Start,
		"/proc/99/net/tcp":                liveTCPHeader + liveTCPRow(45000, 123),
	}
	links := map[string]string{
		"/proc/self/ns/pid": id.PIDNamespace, "/proc/99/ns/pid": id.PIDNamespace,
		"/proc/self/ns/net": "net:[123]", "/proc/99/ns/net": "net:[123]",
		"/proc/99/fd/0": "/dev/pts/1", "/proc/99/fd/3": "socket:[123]",
	}
	p := socketProc{pid: 42,
		read: func(path string, limit int) ([]byte, error) {
			s, ok := files[path]
			if !ok {
				return nil, os.ErrPermission
			}
			return []byte(s), nil
		}, link: func(path string) (string, error) {
			s, ok := links[path]
			if !ok {
				return "", os.ErrPermission
			}
			return s, nil
		}, fds: func(path string) ([]string, error) { return []string{"0", "3"}, nil },
	}
	return p, id, files, links
}

func TestLiveSocketDiscovery(t *testing.T) {
	for _, kind := range []string{"owned", "foreign-inode", "duplicate", "missing", "wildcard", "other-loopback", "connected", "zero-port", "bad-port", "bad-row", "bad-header", "bad-inode", "socket-syntax", "fd-permission", "fd-overflow", "fd-path", "table-permission", "table-overflow", "metadata-overflow", "pid-namespace", "net-namespace", "invalid-net-namespace", "missing-provenance", "wrong-start", "wrong-pid", "wrong-boot", "hidden-proc"} {
		t.Run(kind, func(t *testing.T) {
			p, id, files, links := liveSocketFixture()
			switch kind {
			case "foreign-inode":
				links["/proc/99/fd/3"] = "socket:[999]"
			case "duplicate":
				files["/proc/99/net/tcp"] += liveTCPRow(45001, 123)
			case "missing":
				files["/proc/99/net/tcp"] = liveTCPHeader
			case "wildcard":
				files["/proc/99/net/tcp"] = strings.ReplaceAll(files["/proc/99/net/tcp"], fmt.Sprintf("%08X", binary.NativeEndian.Uint32([]byte{127, 0, 0, 1})), "00000000")
			case "other-loopback":
				files["/proc/99/net/tcp"] = strings.ReplaceAll(files["/proc/99/net/tcp"], fmt.Sprintf("%08X", binary.NativeEndian.Uint32([]byte{127, 0, 0, 1})), fmt.Sprintf("%08X", binary.NativeEndian.Uint32([]byte{127, 0, 0, 2})))
			case "connected":
				files["/proc/99/net/tcp"] = strings.ReplaceAll(files["/proc/99/net/tcp"], " 0A ", " 01 ")
			case "zero-port":
				files["/proc/99/net/tcp"] = liveTCPHeader + liveTCPRow(0, 123)
			case "bad-port":
				files["/proc/99/net/tcp"] = strings.ReplaceAll(files["/proc/99/net/tcp"], ":AFC8", ":FFFFF")
			case "bad-row":
				files["/proc/99/net/tcp"] += "bad\n"
			case "bad-header":
				files["/proc/99/net/tcp"] = "no table"
			case "bad-inode":
				files["/proc/99/net/tcp"] = strings.ReplaceAll(files["/proc/99/net/tcp"], " 123 ", " nope ")
			case "socket-syntax":
				links["/proc/99/fd/3"] = "socket:[123"
			case "fd-permission":
				p.fds = func(string) ([]string, error) { return nil, os.ErrPermission }
			case "fd-overflow":
				p.fds = func(string) ([]string, error) { return make([]string, liveFDLimit+1), nil }
			case "fd-path":
				p.fds = func(string) ([]string, error) { return []string{"../exe"}, nil }
			case "table-permission":
				delete(files, "/proc/99/net/tcp")
			case "table-overflow":
				files["/proc/99/net/tcp"] = strings.Repeat("x", liveTCPBytes+1)
			case "metadata-overflow":
				files["/proc/99/stat"] = strings.Repeat("x", liveProcBytes+1)
			case "pid-namespace":
				links["/proc/99/ns/pid"] = "pid:[999]"
			case "net-namespace":
				links["/proc/99/ns/net"] = "net:[999]"
			case "invalid-net-namespace":
				links["/proc/99/ns/net"], links["/proc/self/ns/net"] = "oops", "oops"
			case "missing-provenance":
				id.PIDNamespace = ""
			case "wrong-start":
				id.Start = "1235"
			case "wrong-pid":
				files["/proc/99/stat"] = strings.Replace(files["/proc/99/stat"], "99 (", "98 (", 1)
			case "wrong-boot":
				id.Boot = "other"
			case "hidden-proc":
				files["/proc/self/mountinfo"] = "1 0 0:1 / /proc rw - proc proc rw,hidepid=2\n"
			}
			socket, err := p.discover(id)
			if kind == "owned" {
				if err != nil || socket.Port != 45000 || socket.Inode != 123 || socket.Process != id || socket.NetworkNamespace != "net:[123]" {
					t.Fatalf("%+v %v", socket, err)
				}
				if err := p.validate(socket); err != nil {
					t.Fatal(err)
				}
				socket.Port++
				if err := p.validate(socket); err == nil {
					t.Fatal("persisted address trusted")
				}
			} else if err == nil {
				t.Fatalf("accepted %s: %+v", kind, socket)
			}
		})
	}
}

func TestLiveSocketDiscoveryRechecks(t *testing.T) {
	for _, kind := range []string{"process-replaced", "namespace-replaced", "fd-closed", "port-replaced", "inode-replaced"} {
		t.Run(kind, func(t *testing.T) {
			p, id, files, links := liveSocketFixture()
			read := p.read
			tables := 0
			p.read = func(path string, limit int) ([]byte, error) {
				b, err := read(path, limit)
				if path == "/proc/99/net/tcp" {
					tables++
					if tables == 1 {
						switch kind {
						case "process-replaced":
							files["/proc/99/stat"] += "0"
						case "namespace-replaced":
							links["/proc/99/ns/net"] = "net:[456]"
						case "fd-closed":
							delete(links, "/proc/99/fd/3")
						case "port-replaced":
							files[path] = liveTCPHeader + liveTCPRow(45001, 123)
						case "inode-replaced":
							files[path] = liveTCPHeader + liveTCPRow(45000, 124)
							links["/proc/99/fd/3"] = "socket:[124]"
						}
					}
				}
				return b, err
			}
			if _, err := p.discover(id); err == nil {
				t.Fatal("race accepted")
			}
		})
	}
}

// P2: a sandboxed service whose proc access to the recorder is denied gets a
// classified refusal (errLiveProcUnreadable), never an accepted socket and
// never a "namespace changed" misreport. A real mismatch stays a mismatch.
func TestLiveSocketProcDeniedIsClassified(t *testing.T) {
	for _, path := range []string{"/proc/99/ns/pid", "/proc/99/ns/net", "/proc/self/ns/net", "/proc/99/fd/3"} {
		t.Run(path, func(t *testing.T) {
			p, id, _, links := liveSocketFixture()
			delete(links, path)
			socket, err := p.discover(id)
			if err == nil {
				t.Fatalf("denied %s accepted: %+v", path, socket)
			}
			if !errors.Is(err, errLiveProcUnreadable) {
				t.Fatalf("denied %s not classified: %v", path, err)
			}
		})
	}
	p, id, _, _ := liveSocketFixture()
	p.fds = func(string) ([]string, error) { return nil, os.ErrPermission }
	if _, err := p.discover(id); !errors.Is(err, errLiveProcUnreadable) {
		t.Fatal("denied fd listing not classified", err)
	}
	p, id, _, links := liveSocketFixture()
	links["/proc/99/ns/net"] = "net:[999]"
	if _, err := p.discover(id); err == nil || errors.Is(err, errLiveProcUnreadable) {
		t.Fatal("namespace mismatch misclassified", err)
	}
}
