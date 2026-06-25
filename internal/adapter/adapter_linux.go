//go:build linux

package adapter

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/truefoundry/aitori/internal/adapter/proc"
)

const (
	linuxCertName    = "aitori"
	linuxCertPath    = "/usr/local/share/ca-certificates/aitori.crt"
	linuxNSSNickname = "aitori"
)

// gnomeIgnore mirrors the macOS bypass list for GNOME's proxy settings.
var gnomeIgnore = []string{"localhost", "127.0.0.0/8", "::1"}

// linuxAdapter implements Adapter on Linux (Tier 1). System trust uses the
// ca-certificates bundle; browser trust additionally uses the NSS store via
// certutil. System proxy is best-effort via GNOME gsettings (there is no
// universal Linux system proxy; CLIs honor HTTPS_PROXY). Requires root for the
// system trust store.
type linuxAdapter struct{ base }

// New returns the Linux adapter.
func New() Adapter { return linuxAdapter{} }

func (linuxAdapter) Name() string { return "linux" }

func (linuxAdapter) InstallCA(certPEM []byte) error {
	if err := os.WriteFile(linuxCertPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write CA to %s (need root?): %w", linuxCertPath, err)
	}
	if out, err := run("update-ca-certificates"); err != nil {
		return fmt.Errorf("update-ca-certificates: %w: %s", err, out)
	}
	installNSS(certPEM) // best-effort browser (NSS) trust
	return nil
}

func (linuxAdapter) UninstallCA() error {
	var firstErr error
	if err := os.Remove(linuxCertPath); err != nil && !os.IsNotExist(err) {
		firstErr = err
	}
	if out, err := run("update-ca-certificates", "--fresh"); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("update-ca-certificates: %w: %s", err, out)
	}
	for _, db := range nssDatabases() {
		_, _ = run("certutil", "-D", "-n", linuxNSSNickname, "-d", db)
	}
	return firstErr
}

func (linuxAdapter) SetSystemProxy(hostPort string) error {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return fmt.Errorf("invalid proxy address %q: %w", hostPort, err)
	}
	if _, err := exec.LookPath("gsettings"); err != nil {
		return fmt.Errorf("gsettings not found; set HTTPS_PROXY=http://%s manually for CLIs/browsers", hostPort)
	}
	steps := [][]string{
		{"set", "org.gnome.system.proxy", "mode", "manual"},
		{"set", "org.gnome.system.proxy.http", "host", host},
		{"set", "org.gnome.system.proxy.http", "port", port},
		{"set", "org.gnome.system.proxy.https", "host", host},
		{"set", "org.gnome.system.proxy.https", "port", port},
		{"set", "org.gnome.system.proxy", "ignore-hosts", gsettingsList(gnomeIgnore)},
	}
	for _, s := range steps {
		if out, err := run("gsettings", s...); err != nil {
			return fmt.Errorf("gsettings %v: %w: %s", s, err, out)
		}
	}
	return nil
}

func (linuxAdapter) ClearSystemProxy() error {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return nil // nothing we set
	}
	if out, err := run("gsettings", "set", "org.gnome.system.proxy", "mode", "none"); err != nil {
		return fmt.Errorf("gsettings reset: %w: %s", err, out)
	}
	return nil
}

func (linuxAdapter) ResolvePID(local, remote netip.AddrPort) (int, error) {
	info, ok := proc.Resolve(local, remote)
	if !ok {
		return 0, fmt.Errorf("no process found for %s<-%s", local, remote)
	}
	return info.PID, nil
}

const nftTable = "aitori"

// StartTransparent installs nftables rules that REDIRECT outbound TCP 80/443 to
// the proxy, excluding the agent's own uid so its gateway dial is not recaptured
// (plan §12). Requires root and nft.
func (linuxAdapter) StartTransparent(cfg TransparentConfig) (TransparentHandle, error) {
	_, port, err := net.SplitHostPort(cfg.ProxyAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy address %q: %w", cfg.ProxyAddr, err)
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return nil, fmt.Errorf("nft not found: install nftables for transparent capture")
	}
	uid := os.Geteuid()
	ruleset := fmt.Sprintf(`table inet %s {
  chain output {
    type nat hook output priority -100; policy accept;
    meta skuid %d return
    tcp dport { 80, 443 } redirect to :%s
  }
}
`, nftTable, uid, port)

	// Startup reconcile: a prior run killed with SIGKILL leaves the table in
	// place, which would make the apply below fail with "file exists". Delete
	// any stale table first (ignore errors — absence is the common case) so a
	// restart cleanly re-establishes capture.
	_, _ = run("nft", "delete", "table", "inet", nftTable)

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("nft apply: %w: %s", err, out)
	}
	return nftHandle{}, nil
}

type nftHandle struct{}

// Close removes the nftables table, fully reverting transparent capture.
func (nftHandle) Close() error {
	if out, err := run("nft", "delete", "table", "inet", nftTable); err != nil {
		return fmt.Errorf("nft delete table: %w: %s", err, out)
	}
	return nil
}

// installNSS adds the CA to available NSS databases (Chrome/Chromium and
// Firefox profiles), best-effort: certutil may be absent.
func installNSS(certPEM []byte) {
	dbs := nssDatabases()
	if len(dbs) == 0 {
		return
	}
	if _, err := exec.LookPath("certutil"); err != nil {
		return
	}
	f, err := os.CreateTemp("", "aitori-ca-*.pem")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(certPEM); err != nil {
		f.Close()
		return
	}
	f.Close()
	for _, db := range dbs {
		_, _ = run("certutil", "-A", "-d", db, "-t", "C,,", "-n", linuxNSSNickname, "-i", f.Name())
	}
}

// nssDatabases returns NSS DB specifiers ("sql:<dir>") for the user's Chrome
// store and any Firefox profiles.
func nssDatabases() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var dbs []string
	if pki := filepath.Join(home, ".pki", "nssdb"); dirExists(pki) {
		dbs = append(dbs, "sql:"+pki)
	}
	ffGlob := filepath.Join(home, ".mozilla", "firefox", "*")
	matches, _ := filepath.Glob(ffGlob)
	for _, m := range matches {
		if dirExists(filepath.Join(m)) && hasNSS(m) {
			dbs = append(dbs, "sql:"+m)
		}
	}
	return dbs
}

func hasNSS(dir string) bool {
	for _, f := range []string{"cert9.db", "cert8.db"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// gsettingsList renders a Go slice as a GVariant string array literal.
func gsettingsList(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = "'" + it + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
