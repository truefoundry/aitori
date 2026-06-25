// Package adapter isolates all OS-specific behavior behind a single interface
// (plan §8.2). Everything else in the codebase is platform-agnostic; only the
// build-tagged adapter_<goos>.go files touch OS APIs.
//
// For milestone M1 the platform methods are stubs that return ErrNotImplemented
// so that explicit-proxy mode (`aitori run`) works everywhere while `aitori
// up` degrades gracefully until the per-OS adapters land (M2–M5).
package adapter

import (
	"errors"
	"net/netip"
)

// ErrNotImplemented is returned by adapter methods that are not yet available
// on the current platform.
var ErrNotImplemented = errors.New("not implemented on this platform yet")

// Adapter abstracts OS trust-store, system-proxy, transparent-capture, PID
// attribution, and secure key storage.
type Adapter interface {
	Name() string

	InstallCA(certPEM []byte) error
	UninstallCA() error

	SetSystemProxy(hostPort string) error
	ClearSystemProxy() error

	StartTransparent(cfg TransparentConfig) (TransparentHandle, error)

	ResolvePID(local, remote netip.AddrPort) (int, error)
}

// TransparentConfig configures Tier-2 transparent capture.
type TransparentConfig struct {
	ProxyAddr      string
	InterceptHosts []string
	// SelfPID must be excluded so the agent's own gateway dial is not recaptured.
	SelfPID int
}

// TransparentHandle controls a running transparent-capture session. Close MUST
// fully revert OS state (fail-open).
type TransparentHandle interface {
	Close() error
}

// base provides ErrNotImplemented defaults; per-OS adapters embed it and
// override the methods they support.
type base struct{}

func (base) InstallCA([]byte) error      { return ErrNotImplemented }
func (base) UninstallCA() error          { return ErrNotImplemented }
func (base) SetSystemProxy(string) error { return ErrNotImplemented }
func (base) ClearSystemProxy() error     { return ErrNotImplemented }
func (base) StartTransparent(TransparentConfig) (TransparentHandle, error) {
	return nil, ErrNotImplemented
}
func (base) ResolvePID(netip.AddrPort, netip.AddrPort) (int, error) { return 0, ErrNotImplemented }
