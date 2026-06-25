//go:build linux

package adapter

import "testing"

func TestGsettingsList(t *testing.T) {
	got := gsettingsList([]string{"localhost", "127.0.0.0/8", "::1"})
	want := "['localhost', '127.0.0.0/8', '::1']"
	if got != want {
		t.Errorf("gsettingsList = %q, want %q", got, want)
	}
	if gsettingsList(nil) != "[]" {
		t.Errorf("empty list = %q, want []", gsettingsList(nil))
	}
}
