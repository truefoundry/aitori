package proxy

import (
	"testing"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/pipeline"
)

// Reload must atomically swap the intercept set so shouldMITM reflects the new
// config, without rebuilding the proxy.
func TestReloadSwapsInterceptSet(t *testing.T) {
	c1 := &config.Config{
		Gateway:        config.GatewayConfig{URL: "https://gw.example/api"},
		InterceptHosts: config.Hosts("api.anthropic.com"),
	}
	p := New(Options{Config: c1, Pipeline: pipeline.New(c1)})

	if !p.shouldMITM("api.anthropic.com:443") {
		t.Fatal("initial config should intercept api.anthropic.com")
	}
	if p.shouldMITM("api.openai.com:443") {
		t.Fatal("initial config should not intercept api.openai.com")
	}
	// The gateway host is never MITM'd (loop prevention).
	if p.shouldMITM("gw.example:443") {
		t.Fatal("gateway host must never be intercepted")
	}

	c2 := &config.Config{
		Gateway:        config.GatewayConfig{URL: "https://gw.example/api"},
		InterceptHosts: config.Hosts("api.openai.com"),
	}
	p.Reload(ReloadOptions{Config: c2, Pipeline: pipeline.New(c2)})

	if p.shouldMITM("api.anthropic.com:443") {
		t.Fatal("after reload, anthropic should no longer be intercepted")
	}
	if !p.shouldMITM("api.openai.com:443") {
		t.Fatal("after reload, openai should be intercepted")
	}
}
