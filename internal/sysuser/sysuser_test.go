package sysuser

import (
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	if got := Expand("/abs/path"); got != "/abs/path" {
		t.Fatalf("absolute path changed: %q", got)
	}
	if got := Expand("relative"); got != "relative" {
		t.Fatalf("relative path changed: %q", got)
	}
	// ToSlash so the suffix check holds regardless of OS path separator.
	if got := filepath.ToSlash(Expand("~/foo")); !strings.HasSuffix(got, "/foo") || strings.HasPrefix(got, "~") {
		t.Fatalf("~ not expanded: %q", got)
	}
}

// Under sudo, Resolve must return SUDO_USER's home and ownership — not root's.
func TestResolveSudoUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SUDO_USER and numeric uid/gid are POSIX-only")
	}
	cur, err := user.Current()
	if err != nil || cur.Username == "root" {
		t.Skip("need a non-root current user")
	}
	t.Setenv("SUDO_USER", cur.Username)
	home, uid, gid := Resolve()
	if home != cur.HomeDir {
		t.Fatalf("home = %q, want %q", home, cur.HomeDir)
	}
	wantUID, _ := strconv.Atoi(cur.Uid)
	wantGID, _ := strconv.Atoi(cur.Gid)
	if uid != wantUID || gid != wantGID {
		t.Fatalf("uid/gid = %d/%d, want %d/%d", uid, gid, wantUID, wantGID)
	}
}

// Without SUDO_USER, Resolve falls back to the current home and sets no owner.
func TestResolveNoSudo(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	home, uid, gid := Resolve()
	if home == "" {
		t.Fatal("expected a home directory")
	}
	if uid != -1 || gid != -1 {
		t.Fatalf("expected no ownership change, got uid/gid %d/%d", uid, gid)
	}
}
