// Package proc resolves a network connection 4-tuple to the owning local
// process (plan §7 "adapter/proc"). It powers PID-based app attribution for
// desktop apps. It is cross-platform via gopsutil; bundle-id extraction is
// macOS-specific (see proc_darwin.go).
//
// Seeing connections owned by other users' processes generally requires
// elevated privileges, so attribution is best-effort: when it cannot determine
// the process, callers fall back to host-based attribution.
package proc

import (
	"net/netip"
	"strings"

	gnet "github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

// Info describes the process that owns a connection.
type Info struct {
	PID      int
	Name     string
	Exe      string
	BundleID string
}

// Resolve finds the local process that owns the connection whose local endpoint
// is remote and whose remote endpoint is local — i.e. local/remote are the
// addresses as seen by the agent's accepted connection (local = the agent's
// side, remote = the peer/client). It returns ok=false when no match is found.
func Resolve(local, remote netip.AddrPort) (Info, bool) {
	conns, err := gnet.Connections("tcp")
	if err != nil {
		return Info{}, false
	}
	for i := range conns {
		c := &conns[i]
		if c.Pid <= 0 {
			continue
		}
		// The client's socket has Laddr == remote (its source) and
		// Raddr == local (the agent).
		if addrMatch(remote, c.Laddr.IP, c.Laddr.Port) && addrMatch(local, c.Raddr.IP, c.Raddr.Port) {
			return enrich(int(c.Pid)), true
		}
	}
	return Info{}, false
}

func enrich(pid int) Info {
	info := Info{PID: pid}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return info
	}
	if name, err := p.Name(); err == nil {
		info.Name = name
	}
	info.Exe = exePath(p)
	if info.Exe != "" {
		info.BundleID = bundleID(info.Exe)
	}
	return info
}

// exePath returns the executable path, falling back to argv[0] on platforms
// where gopsutil cannot read Exe() directly (notably macOS).
func exePath(p *process.Process) string {
	if exe, err := p.Exe(); err == nil && exe != "" {
		return exe
	}
	if argv, err := p.CmdlineSlice(); err == nil && len(argv) > 0 {
		return strings.TrimSpace(argv[0])
	}
	return ""
}

func addrMatch(want netip.AddrPort, ip string, port uint32) bool {
	if uint32(want.Port()) != port {
		return false
	}
	got, err := netip.ParseAddr(ip)
	if err != nil {
		return want.Addr().String() == ip
	}
	return want.Addr().Unmap() == got.Unmap()
}
