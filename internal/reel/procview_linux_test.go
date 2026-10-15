//go:build linux

package reel

import (
	"os"
	"strings"
	"testing"
)

func TestOfflineProcViewRefusesAmbiguity(t *testing.T) {
	for _, kind := range []string{"ordinary", "foreign-pid", "foreign-namespace", "namespace-unreadable", "depth-unreadable", "depth-missing", "depth-duplicate", "mount-unreadable", "hidepid", "subset", "foreign-root", "overmount-pid", "overmount-boot", "no-proc", "duplicate-proc", "bad-boot", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			files := map[string]string{"/proc/self/stat": "42 (test) R", "/proc/self/status": "NSpid:\t42\n", "/proc/self/mountinfo": "1 0 0:1 / /proc rw,nosuid - proc proc rw\n2 1 0:2 / /proc/sys/fs/binfmt_misc rw - binfmt_misc binfmt_misc rw\n", "/proc/sys/kernel/random/boot_id": offlineView.Boot}
			links := map[string]string{"/proc/self/ns/pid": offlineView.Namespace}
			switch kind {
			case "foreign-pid":
				files["/proc/self/stat"] = "142 (test) R"
			case "foreign-namespace":
				files["/proc/self/status"] = "NSpid:\t142\t42\n"
			case "namespace-unreadable":
				delete(links, "/proc/self/ns/pid")
			case "depth-unreadable":
				delete(files, "/proc/self/status")
			case "depth-missing":
				files["/proc/self/status"] = "Name:\ttest\n"
			case "depth-duplicate":
				files["/proc/self/status"] = "NSpid:\t42\nNSpid:\t42\n"
			case "mount-unreadable":
				delete(files, "/proc/self/mountinfo")
			case "hidepid":
				files["/proc/self/mountinfo"] = "1 0 0:1 / /proc rw - proc proc rw,hidepid=2\n"
			case "subset":
				files["/proc/self/mountinfo"] = "1 0 0:1 / /proc rw - proc proc rw,subset=pid\n"
			case "foreign-root":
				files["/proc/self/mountinfo"] = "1 0 0:1 /foreign /proc rw - proc proc rw\n"
			case "overmount-pid":
				files["/proc/self/mountinfo"] += "3 1 0:3 / /proc/101 rw - tmpfs tmpfs rw\n"
			case "overmount-boot":
				files["/proc/self/mountinfo"] += "3 1 0:3 / /proc/sys rw - proc proc rw\n"
			case "no-proc":
				files["/proc/self/mountinfo"] = ""
			case "duplicate-proc":
				files["/proc/self/mountinfo"] += files["/proc/self/mountinfo"]
			case "bad-boot":
				files["/proc/sys/kernel/random/boot_id"] = "other-host"
			case "malformed":
				files["/proc/self/mountinfo"] = "bad"
			}
			read := func(path string) ([]byte, error) {
				value, ok := files[path]
				if !ok {
					return nil, os.ErrPermission
				}
				return []byte(value), nil
			}
			link := func(path string) (string, error) {
				value, ok := links[path]
				if !ok {
					return "", os.ErrPermission
				}
				return value, nil
			}
			view, e := inspectProcView(read, link, 42)
			if kind == "ordinary" {
				if e != nil || view != offlineView {
					t.Fatalf("%+v %v", view, e)
				}
			} else if e == nil {
				t.Fatal("ambiguous proc view accepted")
			}
		})
	}
}
func TestOwnedIdentityToleratesUnestablishedNamespaceProvenance(t *testing.T) {
	// Read-only same-process metadata only: no launch, namespace or signal fixture.
	// An identity recorded under an unsupported proc view carries no namespace;
	// the ownership helper compares what was recorded, never invents provenance.
	id, e := identify(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	unestablished := id
	unestablished.PIDNamespace = ""
	if !owned(unestablished) {
		t.Fatal("identity helper behavior changed")
	}
	if id.PIDNamespace != "" && !namespacePattern.MatchString(id.PIDNamespace) {
		t.Fatal("invalid provenance")
	}
	if len(id.PIDNamespace) > 26 || strings.Contains(id.PIDNamespace, "\n") {
		t.Fatal("unbounded provenance")
	}
}
