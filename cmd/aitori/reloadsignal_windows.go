//go:build windows

package main

import "os"

// reloadSignals is empty on Windows: there is no SIGUSR1, and the console
// control events Go surfaces are reserved for shutdown. Hot-reload via signal
// is unsupported here; restart to pick up config changes.
var reloadSignals []os.Signal
