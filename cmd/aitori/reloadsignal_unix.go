//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reloadSignals are the signals that trigger a governance hot-reload. SIGUSR1
// is Unix-only; SIGHUP is reserved for shutdown so a terminal hangup reverts
// OS state instead of reloading.
var reloadSignals = []os.Signal{syscall.SIGUSR1}
