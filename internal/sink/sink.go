// Package sink provides optional local observability sinks for proxied
// exchanges (plan §7). Sinks are a debug aid, not the governance path, and are
// off by default. They never receive secrets (see capture.Exchange).
package sink

import (
	"github.com/truefoundry/aitori/internal/capture"
	"github.com/truefoundry/aitori/internal/config"
)

// Sink consumes exchange records.
type Sink interface {
	Record(e capture.Exchange)
	Close() error
}

// Multi fans an exchange out to several sinks.
type Multi []Sink

func (m Multi) Record(e capture.Exchange) {
	for _, s := range m {
		s.Record(e)
	}
}

func (m Multi) Close() error {
	var firstErr error
	for _, s := range m {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Build constructs sinks from config. Returns nil when none are configured.
func Build(cfgs []config.SinkConfig) (Sink, error) {
	if len(cfgs) == 0 {
		return nil, nil
	}
	var sinks Multi
	for _, c := range cfgs {
		switch c.Type {
		case "stdout":
			sinks = append(sinks, NewStdout(c.RedactOn()))
		case "file":
			fs, err := NewFile(c.Path, c.RedactOn())
			if err != nil {
				return nil, err
			}
			sinks = append(sinks, fs)
		}
	}
	if len(sinks) == 0 {
		return nil, nil
	}
	return sinks, nil
}

func maybeRedact(e capture.Exchange, redact bool) capture.Exchange {
	if redact {
		return e.Redacted()
	}
	return e
}
