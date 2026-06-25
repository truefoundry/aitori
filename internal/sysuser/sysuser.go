// Package sysuser resolves the invoking user's identity, correctly handling
// sudo. `aitori up` runs as root, but the files it manages (the device CA, the
// token file, the client settings.json) belong in the *invoking* user's home —
// using root's $HOME would misplace them or point config at paths the user
// process can't read. SUDO_USER names that user.
//
// This is the single source of truth for "the user's home", so the CA path
// (config-expanded) and the client config edits (clientcfg) can never diverge.
package sysuser

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Home returns the invoking user's home directory.
func Home() string {
	home, _, _ := Resolve()
	return home
}

// Resolve returns the invoking user's home directory and, on Unix, the uid/gid
// to own files written on their behalf (so root-created files don't stay owned
// by root). Under sudo it resolves SUDO_USER rather than root; otherwise it
// falls back to the current process's home. uid/gid are -1 when ownership
// should not be changed (no SUDO_USER, lookup failure, or Windows).
func Resolve() (home string, uid, gid int) {
	uid, gid = -1, -1
	if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
		if u, err := user.Lookup(su); err == nil {
			if runtime.GOOS != "windows" {
				uid, _ = strconv.Atoi(u.Uid)
				gid, _ = strconv.Atoi(u.Gid)
			}
			return u.HomeDir, uid, gid
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h, uid, gid
	}
	return "", uid, gid
}

// Expand expands a leading "~" in p to the invoking user's home directory. A
// path without a leading "~" is returned unchanged.
func Expand(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home := Home()
	if home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
