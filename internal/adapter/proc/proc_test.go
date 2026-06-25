package proc

import (
	"net/netip"
	"testing"
)

func TestAddrMatch(t *testing.T) {
	ap := netip.MustParseAddrPort("127.0.0.1:8080")
	if !addrMatch(ap, "127.0.0.1", 8080) {
		t.Error("expected match for 127.0.0.1:8080")
	}
	if addrMatch(ap, "127.0.0.1", 9090) {
		t.Error("port mismatch should not match")
	}
	if addrMatch(ap, "10.0.0.1", 8080) {
		t.Error("ip mismatch should not match")
	}
}

func TestAddrMatchIPv6(t *testing.T) {
	ap := netip.MustParseAddrPort("[::1]:443")
	if !addrMatch(ap, "::1", 443) {
		t.Error("expected IPv6 loopback match")
	}
}

func TestAddrMatchV4MappedV6(t *testing.T) {
	// gopsutil may report an IPv4-mapped IPv6 address; Unmap should reconcile.
	ap := netip.MustParseAddrPort("127.0.0.1:8080")
	if !addrMatch(ap, "::ffff:127.0.0.1", 8080) {
		t.Error("expected v4-mapped v6 to match v4")
	}
}
