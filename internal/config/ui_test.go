package config

import "testing"

// The UI defaults to disabled with a listen address ready to use.
func TestUIDefault(t *testing.T) {
	c := Default()
	if c.UI.Enabled {
		t.Error("ui should default to disabled")
	}
	if c.UI.Listen != "127.0.0.1:9100" {
		t.Errorf("ui.listen default = %q, want 127.0.0.1:9100", c.UI.Listen)
	}
}

// --ui / --ui-listen overrides flow through Apply.
func TestUIOverrides(t *testing.T) {
	c := Default()
	on := true
	c.Apply(Overrides{UIEnabled: &on, UIListen: "127.0.0.1:9200"})
	if !c.UI.Enabled {
		t.Error("UIEnabled override not applied")
	}
	if c.UI.Listen != "127.0.0.1:9200" {
		t.Errorf("UIListen override = %q", c.UI.Listen)
	}
}

func TestUIValidate(t *testing.T) {
	// Enabled with no listen → error.
	c := Default()
	c.UI = UIConfig{Enabled: true, Listen: ""}
	if err := c.Validate(); err == nil {
		t.Error("expected error for ui.enabled with empty listen")
	}
	// UI listen colliding with proxy listen → error.
	c = Default()
	c.UI = UIConfig{Enabled: true, Listen: c.Proxy.Listen}
	if err := c.Validate(); err == nil {
		t.Error("expected error for ui.listen == proxy.listen")
	}
	// Distinct → ok.
	c = Default()
	c.UI = UIConfig{Enabled: true, Listen: "127.0.0.1:9100"}
	if err := c.Validate(); err != nil {
		t.Errorf("valid ui config rejected: %v", err)
	}
}
