//go:build !darwin && !linux && !windows

package adapter

import "runtime"

// otherAdapter is a no-op adapter for platforms without a dedicated
// implementation. Explicit-proxy mode still works; `up` reports
// ErrNotImplemented.
type otherAdapter struct{ base }

func (otherAdapter) Name() string { return runtime.GOOS }

// New returns the fallback adapter.
func New() Adapter { return otherAdapter{} }
