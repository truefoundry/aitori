// Package clientcfg manages aitori's edits to AI client config files.
//
// Some clients (notably Node CLIs like Claude Code) ignore the system proxy and
// trust store, so `aitori up` can't govern them by those means alone. Instead
// it writes the proxy/CA environment variables into the client's settings file
// so new sessions route through the proxy and trust the aitori CA; `aitori
// down` (and `up` exit) revert the edit precisely, touching only the keys
// aitori manages.
//
// For each app, aitori prefers the OS-managed (enterprise) settings path when
// it can write it — i.e. running as root/admin under `up`, so the policy is
// enforced and the user can't switch it off — and otherwise falls back to the
// per-user settings file. Apps are listed in registry(); add new ones there.
package clientcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/truefoundry/aitori/internal/sysuser"
)

// managedEnvKeys are the settings.json `env` entries aitori owns. Inject/revert
// touch only these; every other setting is left exactly as the user had it.
var managedEnvKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS"}

// priorEnv records a managed key's pre-inject state so revert can restore it
// exactly (or delete it if it was absent).
type priorEnv struct {
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

// backup is the revert record persisted under ~/.aitori. Path records which
// settings file was patched (managed vs. user) so revert touches the same one.
type backup struct {
	Path            string              `json:"path"`
	SettingsExisted bool                `json:"settings_existed"`
	EnvExisted      bool                `json:"env_existed"`
	Prior           map[string]priorEnv `json:"prior"`
}

// InjectSettings patches one app's settings file with HTTP(S)_PROXY, NO_PROXY,
// and NODE_EXTRA_CA_CERTS. Paths come from the override args when non-empty,
// else the built-in default for appID. proxyAddr is host:port (http:// assumed
// if no scheme); caPath is the aitori CA. It records a backup so the edit can be
// reverted, is idempotent, and returns the settings file written.
func InjectSettings(appID, overrideManagedPath, overrideUserPath, proxyAddr, caPath string) (string, error) {
	home, uid, gid, err := targetUser()
	if err != nil {
		return "", err
	}
	a, ok := defaultApp(appID)
	if !ok {
		a = app{id: appID}
	}
	if overrideManagedPath != "" {
		a.managedPath = overrideManagedPath
	}
	if overrideUserPath != "" {
		a.userRel = overrideUserPath
	}
	target := a.target(home)
	if target == "" {
		return "", fmt.Errorf("no settings path configured for app %q (set inject[].settings)", appID)
	}
	if err := injectAt(target, backupFilePath(home, appID), envWant(proxyAddr, caPath), uid, gid); err != nil {
		return "", err
	}
	return target, nil
}

// RevertAll restores every recorded settings inject to its pre-inject state and
// removes the backups, regardless of the current config. Safe to call when
// nothing was injected (used by `down` and the startup reconcile).
func RevertAll() error {
	home, uid, gid, err := targetUser()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".aitori", "inject")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		bp := filepath.Join(dir, e.Name())
		sp := readBackupPath(bp)
		if sp == "" {
			_ = os.Remove(bp)
			continue
		}
		if rerr := revertAt(sp, bp, uid, gid); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}

// app is an injectable client: a JSON settings file with an `env` block.
type app struct {
	id          string
	managedPath string // absolute OS-managed/enterprise path ("" if none)
	userRel     string // per-user path (home-relative, or absolute)
}

// registry holds built-in default settings paths, keyed by app id.
func registry() []app {
	return []app{
		{
			id:          "claude-code",
			managedPath: claudeCodeManagedPath(),
			userRel:     filepath.Join(".claude", "settings.json"),
		},
	}
}

func defaultApp(id string) (app, bool) {
	for _, a := range registry() {
		if a.id == id {
			return a, true
		}
	}
	return app{}, false
}

// target picks the settings file to patch: the OS-managed path when we can write
// it (root/admin), else the per-user file (absolute, or home-relative).
func (a app) target(home string) string {
	if a.managedPath != "" && canWriteDir(filepath.Dir(a.managedPath)) {
		return a.managedPath
	}
	switch {
	case a.userRel == "":
		return ""
	case filepath.IsAbs(a.userRel):
		return a.userRel
	default:
		return filepath.Join(home, a.userRel)
	}
}

// claudeCodeManagedPath is Claude Code's enterprise managed-settings location,
// which takes precedence over user settings and can't be overridden by the user.
func claudeCodeManagedPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "windows":
		pd := os.Getenv("PROGRAMDATA")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "ClaudeCode", "managed-settings.json")
	default: // linux / wsl
		return "/etc/claude-code/managed-settings.json"
	}
}

func envWant(proxyAddr, caPath string) map[string]string {
	proxyURL := proxyAddr
	if !strings.Contains(proxyURL, "://") {
		proxyURL = "http://" + proxyURL
	}
	return map[string]string{
		"HTTP_PROXY":          proxyURL,
		"HTTPS_PROXY":         proxyURL,
		"NO_PROXY":            "localhost,127.0.0.1,::1",
		"NODE_EXTRA_CA_CERTS": caPath,
	}
}

// canWriteDir reports whether dir can be created and written — the probe for a
// managed path, which needs root/admin.
func canWriteDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".aitori-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// readBackupPath returns the settings path recorded in a backup, or "" if there
// is no backup.
func readBackupPath(backupPath string) string {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return ""
	}
	var b backup
	if json.Unmarshal(data, &b) != nil {
		return ""
	}
	return b.Path
}

// injectAt is the testable core of Inject.
func injectAt(settingsPath, backupPath string, want map[string]string, uid, gid int) error {
	obj, settingsExisted, err := readJSONObject(settingsPath)
	if err != nil {
		return err
	}
	env, envExisted := mapField(obj, "env")

	// Write the backup only on the first inject, so the recorded "prior" state
	// is the true original even across repeated `up` runs.
	if !fileExists(backupPath) {
		b := backup{Path: settingsPath, SettingsExisted: settingsExisted, EnvExisted: envExisted, Prior: map[string]priorEnv{}}
		for _, k := range managedEnvKeys {
			if v, ok := env[k]; ok {
				b.Prior[k] = priorEnv{Present: true, Value: fmt.Sprint(v)}
			} else {
				b.Prior[k] = priorEnv{Present: false}
			}
		}
		if err := writeJSON(backupPath, b, uid, gid); err != nil {
			return fmt.Errorf("write inject backup: %w", err)
		}
	}

	for k, v := range want {
		env[k] = v
	}
	obj["env"] = env
	return writeJSON(settingsPath, obj, uid, gid)
}

// revertAt is the testable core of Revert.
func revertAt(settingsPath, backupPath string, uid, gid int) error {
	data, err := os.ReadFile(backupPath)
	if os.IsNotExist(err) {
		return nil // nothing injected
	}
	if err != nil {
		return err
	}
	var b backup
	if err := json.Unmarshal(data, &b); err != nil {
		return fmt.Errorf("parse inject backup: %w", err)
	}

	// If aitori created settings.json from scratch, remove it entirely.
	if !b.SettingsExisted {
		_ = os.Remove(settingsPath)
		return os.Remove(backupPath)
	}

	obj, _, err := readJSONObject(settingsPath)
	if err != nil {
		return err
	}
	if env, ok := mapField(obj, "env"); ok || len(b.Prior) > 0 {
		for k, prior := range b.Prior {
			if prior.Present {
				env[k] = prior.Value
			} else {
				delete(env, k)
			}
		}
		if len(env) == 0 && !b.EnvExisted {
			delete(obj, "env")
		} else {
			obj["env"] = env
		}
	}
	if err := writeJSON(settingsPath, obj, uid, gid); err != nil {
		return err
	}
	return os.Remove(backupPath)
}

// backupFilePath is the per-app revert record under ~/.aitori/inject/.
func backupFilePath(home, id string) string {
	return filepath.Join(home, ".aitori", "inject", id+".json")
}

// targetUser resolves the home dir and (on Unix) uid/gid to own written files,
// via the shared sysuser resolver — the same one config uses to expand the CA
// path — so settings.json and the CA can never resolve to different homes.
func targetUser() (home string, uid, gid int, err error) {
	home, uid, gid = sysuser.Resolve()
	if home == "" {
		return "", -1, -1, fmt.Errorf("cannot resolve invoking user's home directory")
	}
	return home, uid, gid, nil
}

// mapField returns obj[key] as a string-keyed map, creating an empty one if the
// key is absent or not an object. existed reports whether a usable map was there.
func mapField(obj map[string]any, key string) (m map[string]any, existed bool) {
	if v, ok := obj[key].(map[string]any); ok {
		return v, true
	}
	return map[string]any{}, false
}

func readJSONObject(path string) (obj map[string]any, existed bool, err error) {
	data, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		return map[string]any{}, false, nil
	}
	if rerr != nil {
		return nil, false, rerr
	}
	obj = map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if jerr := json.Unmarshal(data, &obj); jerr != nil {
			return nil, true, fmt.Errorf("parse %s: %w", path, jerr)
		}
	}
	return obj, true, nil
}

func writeJSON(path string, v any, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	chownToUser(path, uid, gid)
	chownToUser(filepath.Dir(path), uid, gid)
	return nil
}

func chownToUser(path string, uid, gid int) {
	if uid >= 0 && gid >= 0 {
		_ = os.Chown(path, uid, gid)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
