package config

import "testing"

func TestUseBuiltinProfilesDefault(t *testing.T) {
	// Unset -> defaults to true (back-compat: built-ins are overlaid).
	var c Config
	if !c.UseBuiltinProfiles() {
		t.Fatal("unset builtin_profiles should default to true")
	}

	f := false
	c.BuiltinProfiles = &f
	if c.UseBuiltinProfiles() {
		t.Fatal("builtin_profiles=false should disable the overlay")
	}

	tr := true
	c.BuiltinProfiles = &tr
	if !c.UseBuiltinProfiles() {
		t.Fatal("builtin_profiles=true should enable the overlay")
	}
}
