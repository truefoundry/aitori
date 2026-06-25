//go:build linux

package proxy

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// soOriginalDst is the getsockopt option that returns the pre-NAT destination
// of a connection redirected by netfilter (REDIRECT/DNAT).
const soOriginalDst = 80

// originalDst returns the original destination ("ip:port") of a transparently
// redirected connection, recovered via SO_ORIGINAL_DST.
func originalDst(conn net.Conn) (string, error) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return "", fmt.Errorf("originalDst: not a TCP connection")
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return "", err
	}

	var addr string
	var opErr error
	if cerr := raw.Control(func(fd uintptr) {
		// SO_ORIGINAL_DST returns a struct sockaddr_in packed into 16 bytes:
		// family(2) | port(2, big-endian) | addr(4) | zero(8).
		mreq, e := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, soOriginalDst)
		if e != nil {
			opErr = e
			return
		}
		b := mreq.Multiaddr
		ip := net.IPv4(b[4], b[5], b[6], b[7])
		port := int(b[2])<<8 | int(b[3])
		addr = net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port))
	}); cerr != nil {
		return "", cerr
	}
	if opErr != nil {
		return "", opErr
	}
	return addr, nil
}
