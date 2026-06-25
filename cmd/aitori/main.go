// Command aitori is the AI traffic governance agent: a selective-MITM proxy
// that reroutes LLM/MCP calls through an AI gateway for governance while leaving
// all other traffic untouched. See the docs for the full design.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/profiles"
	"github.com/truefoundry/aitori/internal/version"
)

var (
	flagConfig      string
	flagVerbose     bool
	flagGatewayURL  string
	flagHeaderCtx   string
	flagTokenFile   string
	flagListen      string
	flagCADir       string
	flagTransparent bool
	flagNoAuth      bool
	flagUI          bool
	flagUIListen    string
)

func main() {
	root := &cobra.Command{
		Use:           "aitori",
		Short:         "AI traffic governance agent",
		Long:          "aitori intercepts local AI app traffic and reroutes LLM/MCP calls through an AI gateway for governance, transparently.",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "path to config file (defaults to built-in defaults overlaid with built-in profiles)")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose (debug) logging")
	root.PersistentFlags().StringVar(&flagGatewayURL, "gateway-url", "", "override gateway.url")
	root.PersistentFlags().StringVar(&flagHeaderCtx, "header-ctx", "", "override gateway.header_ctx (consolidated context header name)")
	root.PersistentFlags().StringVar(&flagTokenFile, "token-file", "", "override gateway.auth.token_file")
	root.PersistentFlags().StringVar(&flagListen, "listen", "", "override proxy.listen (e.g. 127.0.0.1:8080)")
	root.PersistentFlags().StringVar(&flagCADir, "ca-dir", "", "override proxy.ca_dir")
	root.PersistentFlags().BoolVar(&flagTransparent, "transparent", false, "enable transparent capture (override proxy.transparent)")
	root.PersistentFlags().BoolVar(&flagNoAuth, "no-auth", false, "reroute without requiring a gateway token (override gateway.auth.disabled)")
	root.PersistentFlags().BoolVar(&flagUI, "ui", false, "serve the embedded live-traffic UI (override ui.enabled)")
	root.PersistentFlags().StringVar(&flagUIListen, "ui-listen", "", "override ui.listen (e.g. 127.0.0.1:9100)")

	root.AddCommand(
		cmdRun(),
		cmdUp(),
		cmdDown(),
		cmdCA(),
		cmdApps(),
		cmdConfig(),
		cmdStatus(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newLogger returns a stderr text logger at the configured level.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if flagVerbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// loadConfig loads the config file (or defaults), applies command-line
// overrides, and overlays the built-in app profiles. It re-validates at the end
// so overrides and the profile overlay (e.g. the loop-prevention check now also
// covering built-in intercept hosts) are both checked.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(flagConfig)
	if err != nil {
		return nil, err
	}
	cfg.Apply(cliOverrides())
	if cfg.UseBuiltinProfiles() {
		if err := profiles.Apply(cfg); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// cliOverrides snapshots the command-line override flags. Empty strings are
// ignored by Config.Apply; the two booleans only ever force their feature on.
func cliOverrides() config.Overrides {
	o := config.Overrides{
		GatewayURL: flagGatewayURL,
		HeaderCtx:  flagHeaderCtx,
		TokenFile:  flagTokenFile,
		Listen:     flagListen,
		CADir:      flagCADir,
		NoAuth:     flagNoAuth,
		UIListen:   flagUIListen,
	}
	if flagTransparent {
		t := true
		o.Transparent = &t
	}
	if flagUI {
		t := true
		o.UIEnabled = &t
	}
	return o
}
