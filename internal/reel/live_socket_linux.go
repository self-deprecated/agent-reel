//go:build linux

package reel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	liveProcBytes = 1 << 20
	liveTCPBytes  = 4 << 20
	liveFDLimit   = 4096
)

// Proc snapshots detect ordinary exit/replacement races, not hostile same-UID
// ABA or a hostile kernel. Nothing here signals a process or reads its terminal.
// Function seams keep race fixtures independent of the host's proc namespace.
// Production I/O bounds allocations at the source; consumers check bounds too.
type socketProc struct {
	read func(string, int) ([]byte, error)
	link func(string) (string, error)
	fds  func(string) ([]string, error)
	pid  int
}

func discoverRecorderSocket(id ProcessIdentity) (recorderSocket, error) {
	return localSocketProc().discover(id)
}

func validateRecorderSocket(socket recorderSocket) error {
	return localSocketProc().validate(socket)
}

func localSocketProc() socketProc {
	return socketProc{read: func(path string, limit int) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, int64(limit)+1))
	}, link: os.Readlink, fds: func(path string) ([]string, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		names, err := f.Readdirnames(liveFDLimit + 1)
		if err == io.EOF {
			err = nil
		}
		return names, err
	}, pid: os.Getpid()}
}

// procDenied classifies a failed proc access. A permission refusal (a
// sandboxed service in its own user namespace cannot read another process's
// ns/ or fd/ links) is reported as errLiveProcUnreadable, so the library can
// say why Live is unavailable; it is still a refusal and nothing is guessed.
// Any other failure keeps its specific message.
func procDenied(err error, otherwise error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %w", errLiveProcUnreadable, err)
	}
	return otherwise
}

func (p socketProc) bounded(path string, limit int) ([]byte, error) {
	b, err := p.read(path, limit)
	if len(b) > limit {
		return nil, fmt.Errorf("live proc data exceeds bound")
	}
	return b, err
}

func (p socketProc) identity(id ProcessIdentity) (string, error) {
	start, parseErr := strconv.ParseUint(id.Start, 10, 64)
	if id.PID < 2 || id.PID > 2147483647 || parseErr != nil || start == 0 || strconv.FormatUint(start, 10) != id.Start || !bootPattern.MatchString(id.Boot) || !namespacePattern.MatchString(id.PIDNamespace) {
		return "", fmt.Errorf("live process provenance missing or invalid")
	}
	view, err := inspectProcView(func(path string) ([]byte, error) {
		return p.bounded(path, liveProcBytes)
	}, p.link, p.pid)
	if err != nil {
		return "", err
	}
	if view.Boot != id.Boot || view.Namespace != id.PIDNamespace {
		return "", fmt.Errorf("live process provenance changed")
	}
	base := "/proc/" + strconv.Itoa(id.PID)
	pidns, err := p.link(base + "/ns/pid")
	if err != nil {
		return "", procDenied(err, fmt.Errorf("live target PID namespace changed"))
	}
	if pidns != id.PIDNamespace {
		return "", fmt.Errorf("live target PID namespace changed")
	}
	selfnet, err := p.link("/proc/self/ns/net")
	if err != nil {
		return "", procDenied(err, fmt.Errorf("live network namespace unavailable"))
	}
	if !validNetNamespace(selfnet) {
		return "", fmt.Errorf("live network namespace unavailable")
	}
	netns, err := p.link(base + "/ns/net")
	if err != nil {
		return "", procDenied(err, fmt.Errorf("live target network namespace differs"))
	}
	if netns != selfnet {
		return "", fmt.Errorf("live target network namespace differs")
	}
	stat, err := p.bounded(base+"/stat", liveProcBytes)
	if err != nil {
		return "", procDenied(err, err)
	}
	s := string(stat)
	open, close := strings.Index(s, " ("), strings.LastIndex(s, ")")
	if open < 1 || close <= open || s[:open] != strconv.Itoa(id.PID) {
		return "", fmt.Errorf("invalid live target stat")
	}
	fields := strings.Fields(s[close+1:])
	if len(fields) < 20 || fields[19] != id.Start {
		return "", fmt.Errorf("live process start changed")
	}
	return netns, nil
}

func validNetNamespace(s string) bool {
	return strings.HasPrefix(s, "net:[") && namespacePattern.MatchString("pid:"+strings.TrimPrefix(s, "net:"))
}

func (p socketProc) snapshot(id ProcessIdentity) (recorderSocket, error) {
	var zero recorderSocket
	ns, err := p.identity(id)
	if err != nil {
		return zero, err
	}
	base := "/proc/" + strconv.Itoa(id.PID)
	fds, err := p.fds(base + "/fd")
	if err != nil {
		return zero, procDenied(err, err)
	}
	if len(fds) > liveFDLimit {
		return zero, fmt.Errorf("live FD count exceeds bound")
	}
	owned := map[uint64]bool{}
	for _, fd := range fds {
		if n, err := strconv.ParseUint(fd, 10, 32); err != nil || strconv.FormatUint(n, 10) != fd {
			return zero, fmt.Errorf("invalid live FD")
		}
		target, err := p.link(base + "/fd/" + fd)
		if err != nil {
			return zero, procDenied(err, err)
		} // Concurrent close is unavailable, not guessed.
		if !strings.HasPrefix(target, "socket:[") {
			continue
		}
		inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]"), 10, 64)
		if err != nil || inode == 0 || target != fmt.Sprintf("socket:[%d]", inode) {
			return zero, fmt.Errorf("invalid live socket inode")
		}
		owned[inode] = true
	}
	table, err := p.bounded(base+"/net/tcp", liveTCPBytes)
	if err != nil {
		return zero, procDenied(err, err)
	}
	port, inode, err := ownedLoopbackListener(table, owned)
	if err != nil {
		return zero, err
	}
	after, err := p.identity(id)
	if err != nil {
		return zero, err
	}
	if after != ns {
		return zero, fmt.Errorf("live network namespace changed")
	}
	return recorderSocket{Process: id, NetworkNamespace: ns, Inode: inode, Port: port}, nil
}

func ownedLoopbackListener(table []byte, owned map[uint64]bool) (uint16, uint64, error) {
	lines := strings.Split(strings.TrimSpace(string(table)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "sl") || !strings.Contains(lines[0], "local_address") {
		return 0, 0, fmt.Errorf("invalid live TCP table")
	}
	// /proc/net/tcp renders the address as a native-endian u32, including on
	// big-endian Linux. The port, unlike the address, is a numeric hex value.
	loopback := fmt.Sprintf("%08X", binary.NativeEndian.Uint32([]byte{127, 0, 0, 1}))
	var port uint16
	var inode uint64
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 10 {
			return 0, 0, fmt.Errorf("invalid live TCP row")
		}
		n, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid live TCP inode")
		}
		if !owned[n] || f[3] != "0A" {
			continue
		}
		address := strings.Split(f[1], ":")
		if len(address) != 2 {
			return 0, 0, fmt.Errorf("invalid live TCP address")
		}
		if address[0] != loopback {
			continue
		}
		v, err := strconv.ParseUint(address[1], 16, 16)
		if err != nil || len(address[1]) != 4 || v == 0 {
			return 0, 0, fmt.Errorf("invalid live TCP port")
		}
		if port != 0 {
			return 0, 0, fmt.Errorf("ambiguous live listener")
		}
		port, inode = uint16(v), n
	}
	if port == 0 {
		return 0, 0, fmt.Errorf("owned live listener unavailable")
	}
	return port, inode, nil
}

func (p socketProc) discover(id ProcessIdentity) (recorderSocket, error) {
	first, err := p.snapshot(id)
	if err != nil {
		return recorderSocket{}, err
	}
	second, err := p.snapshot(id)
	if err != nil {
		return recorderSocket{}, err
	}
	if first != second {
		return recorderSocket{}, fmt.Errorf("live socket changed during discovery")
	}
	return first, nil
}

func (p socketProc) validate(socket recorderSocket) error {
	actual, err := p.discover(socket.Process)
	if err != nil {
		return err
	}
	if actual != socket {
		return fmt.Errorf("live socket ownership changed")
	}
	return nil
}
