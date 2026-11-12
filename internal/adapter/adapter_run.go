package adapter

import (
	"fmt"
	"os/exec"
)

// allowedCommands is the fixed allow-list of executables the adapters are
// permitted to invoke. run() is always called with a literal command name
// from the platform adapters, never with user- or config-supplied input;
// this allow-list makes that invariant explicit and enforced.
var allowedCommands = map[string]bool{
	"update-ca-certificates": true,
	"certutil":               true,
	"gsettings":               true,
	"nft":                     true,
	"security":                true,
	"networksetup":            true,
}

// run executes a command and returns its combined output. It is the small shell
// shim used by the platform adapters (security, networksetup, certutil, ...).
func run(name string, args ...string) ([]byte, error) {
	if !allowedCommands[name] {
		return nil, fmt.Errorf("adapter: refusing to run unrecognized command %q", name)
	}
	return exec.Command(name, args...).CombinedOutput()
}
