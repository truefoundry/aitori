package adapter

import "os/exec"

// run executes a command and returns its combined output. It is the small shell
// shim used by the platform adapters (security, networksetup, certutil, ...).
func run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
