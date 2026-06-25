//go:build !linux

package proxy

import (
	"fmt"
	"net"
	"runtime"
)

// originalDst is only implemented on Linux (SO_ORIGINAL_DST). Transparent
// capture on other platforms relies on a native component that supplies the
// destination differently (see README).
func originalDst(net.Conn) (string, error) {
	return "", fmt.Errorf("originalDst: transparent capture not supported on %s", runtime.GOOS)
}
