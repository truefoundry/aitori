//go:build darwin

package adapter

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/truefoundry/aitori/internal/adapter/proc"
)

// macOS-specific constants. caCommonName MUST match the CN aitori uses when
// creating its CA (see internal/ca: "aitori Device CA").
const (
	caCommonName   = "aitori Device CA"
	systemKeychain = "/Library/Keychains/System.keychain"
)

// proxyBypass keeps loopback and link-local traffic off the proxy.
var proxyBypass = []string{"localhost", "127.0.0.1", "::1", "*.local", "169.254/16"}

// darwinAdapter implements Adapter on macOS (Tier 1). CA trust uses the System
// keychain and system proxy uses networksetup, both of which require running as
// root (e.g. via sudo) — installers run with elevated rights (plan §14).
type darwinAdapter struct{ base }

// New returns the macOS adapter.
func New() Adapter { return darwinAdapter{} }

func (darwinAdapter) Name() string { return "darwin" }

func (darwinAdapter) InstallCA(certPEM []byte) error {
	f, err := os.CreateTemp("", "aitori-ca-*.pem")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(certPEM); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// -d: admin (system) cert store; -r trustRoot: trust as a root CA.
	if out, err := run("security", "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", systemKeychain, f.Name()); err != nil {
		return fmt.Errorf("install CA into System keychain (need root?): %w: %s", err, out)
	}
	return nil
}

func (darwinAdapter) UninstallCA() error {
	// Export the installed cert (if present) so we can also drop its admin trust
	// settings.
	pemOut, err := run("security", "find-certificate", "-c", caCommonName, "-p", systemKeychain)
	if err != nil {
		return nil // not installed; nothing to do
	}
	if f, ferr := os.CreateTemp("", "aitori-ca-*.pem"); ferr == nil {
		defer os.Remove(f.Name())
		if _, werr := f.Write(pemOut); werr == nil {
			f.Close()
			_, _ = run("security", "remove-trusted-cert", "-d", f.Name())
		} else {
			f.Close()
		}
	}
	if out, derr := run("security", "delete-certificate", "-c", caCommonName, "-t", systemKeychain); derr != nil {
		return fmt.Errorf("delete CA from System keychain (need root?): %w: %s", derr, out)
	}
	return nil
}

func (darwinAdapter) SetSystemProxy(hostPort string) error {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return fmt.Errorf("invalid proxy address %q: %w", hostPort, err)
	}
	services, err := networkServices()
	if err != nil {
		return err
	}
	for _, svc := range services {
		if out, err := run("networksetup", "-setwebproxy", svc, host, port); err != nil {
			return fmt.Errorf("set http proxy on %q: %w: %s", svc, err, out)
		}
		if out, err := run("networksetup", "-setsecurewebproxy", svc, host, port); err != nil {
			return fmt.Errorf("set https proxy on %q: %w: %s", svc, err, out)
		}
		args := append([]string{"-setproxybypassdomains", svc}, proxyBypass...)
		if out, err := run("networksetup", args...); err != nil {
			return fmt.Errorf("set proxy bypass on %q: %w: %s", svc, err, out)
		}
	}
	return nil
}

func (darwinAdapter) ClearSystemProxy() error {
	services, err := networkServices()
	if err != nil {
		return err
	}
	var firstErr error
	for _, svc := range services {
		if out, err := run("networksetup", "-setwebproxystate", svc, "off"); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("disable http proxy on %q: %w: %s", svc, err, out)
		}
		if out, err := run("networksetup", "-setsecurewebproxystate", svc, "off"); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("disable https proxy on %q: %w: %s", svc, err, out)
		}
	}
	return firstErr
}

func (darwinAdapter) ResolvePID(local, remote netip.AddrPort) (int, error) {
	info, ok := proc.Resolve(local, remote)
	if !ok {
		return 0, fmt.Errorf("no process found for %s<-%s", local, remote)
	}
	return info.PID, nil
}

// networkServices returns the enabled (non-disabled) network service names.
func networkServices() ([]string, error) {
	out, err := run("networksetup", "-listallnetworkservices")
	if err != nil {
		return nil, fmt.Errorf("list network services: %w: %s", err, out)
	}
	return parseNetworkServices(string(out)), nil
}

// parseNetworkServices parses `networksetup -listallnetworkservices` output,
// skipping the header line and disabled services (prefixed with "*").
func parseNetworkServices(out string) []string {
	var services []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "An asterisk") {
			continue
		}
		if strings.HasPrefix(line, "*") {
			continue // disabled
		}
		services = append(services, line)
	}
	return services
}
