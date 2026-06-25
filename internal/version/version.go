// Package version exposes the agent's build version.
package version

// Version is the semantic version of the agent. It is overridden at build time
// via -ldflags "-X github.com/truefoundry/aitori/internal/version.Version=x.y.z".
var Version = "0.0.0-dev"
