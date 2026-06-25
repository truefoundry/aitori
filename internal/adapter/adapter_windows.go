//go:build windows

package adapter

import (
	"fmt"
	"net/netip"
	"os"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/truefoundry/aitori/internal/adapter/proc"
)

const (
	winCertCommonName = "aitori Device CA"
	inetSettingsKey   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
)

// WinINET option codes for notifying of proxy changes.
const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

var (
	wininet                = windows.NewLazySystemDLL("wininet.dll")
	procInternetSetOptionW = wininet.NewProc("InternetSetOptionW")
)

// windowsAdapter implements Adapter on Windows (Tier 1). CA trust uses the
// LocalMachine Root store via certutil (requires admin); the system proxy uses
// the WinINET registry settings (per-user). PID attribution is unavailable
// (gopsutil cannot enumerate connections on Windows), so attribution falls back
// to host-based matching.
type windowsAdapter struct{ base }

// New returns the Windows adapter.
func New() Adapter { return windowsAdapter{} }

func (windowsAdapter) Name() string { return "windows" }

func (windowsAdapter) InstallCA(certPEM []byte) error {
	f, err := os.CreateTemp("", "aitori-ca-*.crt")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(certPEM); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if out, err := run("certutil", "-addstore", "-f", "Root", f.Name()); err != nil {
		return fmt.Errorf("add CA to Root store (need admin?): %w: %s", err, out)
	}
	return nil
}

func (windowsAdapter) UninstallCA() error {
	if out, err := run("certutil", "-delstore", "Root", winCertCommonName); err != nil {
		return fmt.Errorf("remove CA from Root store (need admin?): %w: %s", err, out)
	}
	return nil
}

func (windowsAdapter) SetSystemProxy(hostPort string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open Internet Settings: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue("ProxyServer", hostPort); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyOverride", "localhost;127.*;<local>"); err != nil {
		return err
	}
	if err := key.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}
	notifyWinINET()
	return nil
}

func (windowsAdapter) ClearSystemProxy() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return nil // nothing to clear
	}
	defer key.Close()
	if err := key.SetDWordValue("ProxyEnable", 0); err != nil {
		return err
	}
	notifyWinINET()
	return nil
}

func (windowsAdapter) ResolvePID(local, remote netip.AddrPort) (int, error) {
	info, ok := proc.Resolve(local, remote)
	if !ok {
		return 0, fmt.Errorf("no process found for %s<-%s", local, remote)
	}
	return info.PID, nil
}

// notifyWinINET tells WinINET that proxy settings changed so they take effect
// without a logoff.
func notifyWinINET() {
	procInternetSetOptionW.Call(0, internetOptionSettingsChanged, 0, 0)
	procInternetSetOptionW.Call(0, internetOptionRefresh, 0, 0)
}
