package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/truefoundry/aitori/internal/adapter"
	"github.com/truefoundry/aitori/internal/adapter/proc"
	"github.com/truefoundry/aitori/internal/ca"
	"github.com/truefoundry/aitori/internal/circuit"
	"github.com/truefoundry/aitori/internal/clientcfg"
	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/liveui"
	"github.com/truefoundry/aitori/internal/pipeline"
	"github.com/truefoundry/aitori/internal/proxy"
	"github.com/truefoundry/aitori/internal/router"
	"github.com/truefoundry/aitori/internal/sink"
	"github.com/truefoundry/aitori/internal/token"
	"github.com/truefoundry/aitori/internal/version"
)

// agent bundles the wired-up runtime components.
type agent struct {
	cfg     *config.Config
	ca      *ca.CA
	token   *token.FileSource
	proxy   *proxy.Proxy
	sink    sink.Sink
	ui      *liveui.Server
	tHandle adapter.TransparentHandle
}

func (a *agent) close() {
	if a.token != nil {
		a.token.Close()
	}
	if a.sink != nil {
		a.sink.Close()
	}
}

func buildAgent(cmd *cobra.Command) (*agent, error) {
	log := newLogger()
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	dca, err := ca.LoadOrCreate(cfg.Proxy.CADir, ca.Options{Organization: "aitori"})
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}

	tok, err := token.NewFileSource(cfg.Gateway.Auth.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("watch token file: %w", err)
	}

	rtr, err := buildRouter(cfg, tok)
	if err != nil {
		return nil, err
	}

	snk, err := sink.Build(cfg.Sinks)
	if err != nil {
		return nil, fmt.Errorf("build sinks: %w", err)
	}

	// The live UI is just another sink (in-memory + SSE). Combine it with the
	// configured sinks into one recorder; the agent owns its Close.
	var ui *liveui.Server
	recorder := snk
	if cfg.UI.Enabled {
		ui = liveui.New(true) // redact: drop query strings, like the other sinks
		fanout := sink.Multi{ui}
		if snk != nil {
			fanout = append(fanout, snk)
		}
		recorder = fanout
	}

	opts := proxy.Options{
		Config:   cfg,
		CA:       dca,
		Router:   rtr,
		Pipeline: pipeline.New(cfg),
		Logger:   log,
		Proc: func(local, remote netip.AddrPort) (proxy.ProcInfo, bool) {
			info, ok := proc.Resolve(local, remote)
			if !ok {
				return proxy.ProcInfo{}, false
			}
			return proxy.ProcInfo{PID: info.PID, Name: info.Name, Exe: info.Exe, BundleID: info.BundleID}, true
		},
	}
	if recorder != nil {
		opts.Recorder = recorder
	}
	px := proxy.New(opts)

	return &agent{cfg: cfg, ca: dca, token: tok, proxy: px, sink: recorder, ui: ui}, nil
}

// buildRouter constructs the gateway router from cfg, sharing the given token
// source so the hot-reloading token watcher is not duplicated on a SIGHUP
// rebuild. A fresh breaker is intentional — a reload resets gateway health.
func buildRouter(cfg *config.Config, tok *token.FileSource) (*router.Router, error) {
	gwURL, err := cfg.GatewayURL()
	if err != nil {
		return nil, err
	}
	return router.New(router.Options{
		GatewayURL:   gwURL,
		Token:        tok,
		Breaker:      circuit.New(5, 30*time.Second),
		HeaderToken:  cfg.Gateway.HeaderToken,
		HeaderOrig:   cfg.Gateway.HeaderOrig,
		HeaderCtx:    cfg.Gateway.HeaderCtx,
		OnError:      cfg.Gateway.OnError,
		AuthDisabled: cfg.Gateway.Auth.Disabled,
		AgentVersion: version.Version,
		Headers:      cfg.Gateway.Headers,
		DeviceHost:   deviceHostname(),
		DeviceOS:     runtime.GOOS,
	}), nil
}

// deviceHostname returns the machine's hostname for the device metadata header,
// or "" if it can't be determined (best-effort, never fatal).
func deviceHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// serve binds the listener and serves until SIGINT/SIGTERM/SIGHUP. When withOS
// is true it also installs the CA and sets the system proxy via the adapter,
// reverting both on exit (fail-open). SIGHUP is treated as shutdown — not
// reload — so closing the controlling terminal (which delivers SIGHUP) cleanly
// reverts the system proxy instead of leaving it dangling.
func (a *agent) serve(withOS bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// SIGUSR1 hot-reloads governance (intercept set, rules, gateway, pipeline)
	// from a fresh config without dropping connections or touching OS state
	// (system proxy, CA, injects). Token changes already hot-reload via fsnotify.
	// The signal is Unix-only (reloadSignals is empty on Windows).
	reloadSig := make(chan os.Signal, 1)
	if len(reloadSignals) > 0 {
		signal.Notify(reloadSig, reloadSignals...)
		defer signal.Stop(reloadSig)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadSig:
				a.reload()
			}
		}
	}()

	ln, err := net.Listen("tcp", a.cfg.Proxy.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.cfg.Proxy.Listen, err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	// The embedded live-traffic UI runs on its own listener, alongside the proxy.
	var uiSrv *http.Server
	if a.ui != nil {
		uiLn, err := net.Listen("tcp", a.cfg.UI.Listen)
		if err != nil {
			return fmt.Errorf("ui listen %s: %w", a.cfg.UI.Listen, err)
		}
		uiSrv = &http.Server{Handler: a.ui.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := uiSrv.Serve(uiLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "aitori: live UI server error:", err)
			}
		}()
	}

	transparent := false
	if withOS {
		ad := adapter.New()
		if a.cfg.Proxy.Transparent {
			transparent = a.setupTransparent(ad, addr)
		}
		if !transparent {
			revert := a.setupOS(ad, addr)
			defer revert()
		} else {
			defer a.revertTransparent()
		}
	}

	a.printStartup(addr, withOS, transparent)

	srvErr := make(chan error, 1)
	go func() {
		if transparent {
			srvErr <- a.proxy.ServeTransparent(ln)
		} else {
			srvErr <- a.proxy.Serve(ln)
		}
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "aitori: shutting down (reverting OS state if any)")
		shctx, cancel := context.WithTimeout(context.Background(), a.cfg.DrainTimeout())
		defer cancel()
		if uiSrv != nil {
			_ = uiSrv.Shutdown(shctx)
		}
		_ = a.proxy.Shutdown(shctx)
		return nil
	case err := <-srvErr:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

// reload rebuilds the governance state from a fresh config and swaps it into the
// running proxy atomically. The listener and OS state (system proxy, CA,
// injects) are deliberately left untouched — changing those needs a restart.
// On any error the current state is kept (fail-safe: never drop governance).
func (a *agent) reload() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aitori: SIGUSR1 reload failed (keeping current config):", err)
		return
	}
	rtr, err := buildRouter(cfg, a.token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aitori: SIGUSR1 reload failed (keeping current config):", err)
		return
	}
	a.proxy.Reload(proxy.ReloadOptions{Config: cfg, Router: rtr, Pipeline: pipeline.New(cfg)})
	fmt.Fprintf(os.Stderr, "aitori: reloaded config (%d intercept host pattern(s))\n", len(cfg.InterceptHosts))
}

// setupOS best-effort installs the CA + system proxy and returns a revert func.
// On platforms where the adapter is not yet implemented it prints a clear
// notice and continues in explicit-proxy mode.
func (a *agent) setupOS(ad adapter.Adapter, addr string) func() {
	// Startup reconcile: a prior run killed with SIGKILL (no `down`) can leave
	// settings injects behind — including for apps since dropped from the config,
	// which the apply below would never touch. RevertAll scans the backup dir and
	// undoes every recorded inject before we re-apply the current set, so residue
	// never accumulates across unclean restarts.
	if err := clientcfg.RevertAll(); err != nil {
		fmt.Fprintln(os.Stderr, "aitori: WARNING startup reconcile (revert stale injects):", err)
	}

	if err := ad.InstallCA(a.ca.CertPEM()); err != nil {
		a.osNotice("install CA", err)
		return func() {}
	}

	// Once we touch the system proxy we must ALWAYS clear it on the way out — a
	// proxy left pointing at a stopped aitori breaks all of the device's
	// traffic. This runs even if SetSystemProxy fails partway across multiple
	// network services (clearing is idempotent: services without a proxy are
	// left untouched). The CA is intentionally left installed.
	clearProxy := func() {
		if err := ad.ClearSystemProxy(); err != nil {
			fmt.Fprintln(os.Stderr, "aitori: WARNING failed to clear system proxy:", err)
		} else {
			fmt.Fprintln(os.Stderr, "aitori: system proxy cleared")
		}
	}

	if err := ad.SetSystemProxy(addr); err != nil {
		a.osNotice("set system proxy", err)
		clearProxy() // undo any services set before the failure
		_ = ad.UninstallCA()
		return func() {}
	}
	fmt.Fprintf(os.Stderr, "aitori: installed CA and set system proxy to %s\n", addr)

	// Node-based clients (e.g. Claude Code) ignore the system proxy/trust store,
	// so point them at the proxy explicitly per the inject block. Reverted on
	// exit / `down`.
	for _, line := range applyInjections(a.cfg, addr, ca.CertPath(a.cfg.Proxy.CADir)) {
		fmt.Fprintf(os.Stderr, "aitori: configured %s\n", line)
	}

	return func() {
		clearProxy()
		revertClientConfig(a.cfg)
	}
}

// applyInjections applies every enabled settings-mode inject entry via clientcfg
// — patching the app's managed/user settings env with proxy + CA vars so a
// client that ignores the system proxy (e.g. Claude Code) routes through the
// Tier-1 proxy. Returns one human line per applied entry.
func applyInjections(cfg *config.Config, addr, caPath string) []string {
	var applied []string
	for _, e := range cfg.Inject {
		if !e.On() {
			continue
		}
		mp, up := "", ""
		if e.Settings != nil {
			mp, up = e.Settings.ManagedPath, e.Settings.UserPath
		}
		target, err := clientcfg.InjectSettings(e.App, mp, up, addr, caPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aitori: WARNING inject %q (settings): %v\n", e.App, err)
			continue
		}
		applied = append(applied, e.App+" (settings) -> "+target)
	}
	return applied
}

// revertClientConfig reverts all settings injects. The arg is unused (every
// recorded inject is reverted regardless of config); kept for call-site clarity.
func revertClientConfig(_ *config.Config) {
	if err := clientcfg.RevertAll(); err != nil {
		fmt.Fprintln(os.Stderr, "aitori: WARNING failed to revert client app settings:", err)
	}
}

// setupTransparent attempts Tier-2 transparent capture. It returns true on
// success; on failure it prints a notice and the caller falls back to the
// system proxy.
func (a *agent) setupTransparent(ad adapter.Adapter, addr string) bool {
	h, err := ad.StartTransparent(adapter.TransparentConfig{
		ProxyAddr:      addr,
		InterceptHosts: a.cfg.InterceptHosts.Patterns(),
		SelfPID:        os.Getpid(),
	})
	if err != nil {
		if errors.Is(err, adapter.ErrNotImplemented) {
			fmt.Fprintf(os.Stderr, "aitori: transparent capture is not available on %s yet; falling back to system proxy.\n", ad.Name())
		} else {
			fmt.Fprintf(os.Stderr, "aitori: WARNING transparent setup failed: %v; falling back to system proxy.\n", err)
		}
		return false
	}
	a.tHandle = h
	fmt.Fprintf(os.Stderr, "aitori: transparent capture active (redirecting to %s)\n", addr)
	return true
}

func (a *agent) revertTransparent() {
	if a.tHandle == nil {
		return
	}
	if err := a.tHandle.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "aitori: WARNING failed to revert transparent capture:", err)
	}
}

func (a *agent) osNotice(op string, err error) {
	if errors.Is(err, adapter.ErrNotImplemented) {
		fmt.Fprintf(os.Stderr, "aitori: automatic OS setup (%s) is not available on this platform yet; running in explicit-proxy mode.\n", op)
		fmt.Fprintf(os.Stderr, "         Trust the CA at %s and point your app/browser proxy at the address below.\n", ca.CertPath(a.cfg.Proxy.CADir))
		return
	}
	fmt.Fprintf(os.Stderr, "aitori: WARNING could not %s: %v (continuing in explicit-proxy mode)\n", op, err)
}

func (a *agent) printStartup(addr string, withOS, transparent bool) {
	fmt.Fprintf(os.Stderr, "aitori %s listening on %s\n", version.Version, addr)
	fmt.Fprintf(os.Stderr, "  CA certificate: %s\n", ca.CertPath(a.cfg.Proxy.CADir))
	if a.cfg.Gateway.URL != "" {
		fmt.Fprintf(os.Stderr, "  gateway:        %s (%s)\n", a.cfg.Gateway.URL, a.cfg.Gateway.OnError)
	} else {
		fmt.Fprintln(os.Stderr, "  gateway:        (none — inspect & passthrough: calls are decrypted, classified, and forwarded unchanged)")
	}
	if a.cfg.Gateway.Auth.Disabled {
		fmt.Fprintln(os.Stderr, "  token:          (auth disabled — rerouting without a token)")
	} else {
		fmt.Fprintf(os.Stderr, "  token:          %s (%s)\n", a.cfg.Gateway.Auth.TokenFile, a.token.State())
	}
	fmt.Fprintf(os.Stderr, "  intercepting:   %d host pattern(s)\n", len(a.cfg.InterceptHosts))
	if a.ui != nil {
		fmt.Fprintf(os.Stderr, "  live UI:        http://%s/\n", a.cfg.UI.Listen)
	}
	switch {
	case transparent:
		fmt.Fprintln(os.Stderr, "  mode:           transparent capture (Tier 2)")
	case withOS:
		fmt.Fprintln(os.Stderr, "  mode:           system proxy (Tier 1)")
	default:
		fmt.Fprintln(os.Stderr, "  mode:           explicit proxy (trust the CA above and configure your client to use this proxy)")
	}
}

func cmdRun() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run in explicit-proxy mode (no OS changes)",
		Long:  "Run the proxy without modifying OS state. Trust the CA and configure your client to use this proxy manually.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := buildAgent(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			return a.serve(false)
		},
	}
}

func cmdUp() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Install CA, set the system proxy, and run",
		Long:  "Install the per-device CA into the OS trust store, set the system proxy, and run. Reverts on exit (incl. Ctrl-C).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := buildAgent(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			return a.serve(true)
		},
	}
}

func cmdDown() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Revert system proxy / transparent capture",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ad := adapter.New()
			// Always attempt to revert client-config injects, even if the OS proxy
			// adapter is unimplemented — they may have happened on a prior run and
			// must be undone. Config is best-effort (settings revert needs none).
			cfg, _ := loadConfig()
			revertClientConfig(cfg)
			if err := ad.ClearSystemProxy(); err != nil {
				if errors.Is(err, adapter.ErrNotImplemented) {
					fmt.Fprintf(os.Stderr, "aitori: nothing to revert (OS integration not available on %s yet)\n", ad.Name())
					return nil
				}
				return err
			}
			fmt.Fprintln(os.Stderr, "aitori: system proxy cleared")
			return nil
		},
	}
}

func cmdCA() *cobra.Command {
	c := &cobra.Command{Use: "ca", Short: "Manage the per-device CA"}

	install := &cobra.Command{
		Use:   "install",
		Short: "Generate (if needed) and install the CA into the OS trust store",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			dca, err := ca.LoadOrCreate(cfg.Proxy.CADir, ca.Options{Organization: "aitori"})
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "aitori: CA certificate at %s\n", ca.CertPath(cfg.Proxy.CADir))
			ad := adapter.New()
			if err := ad.InstallCA(dca.CertPEM()); err != nil {
				if errors.Is(err, adapter.ErrNotImplemented) {
					fmt.Fprintf(os.Stderr, "aitori: automatic install not available on %s yet; trust the certificate above manually.\n", ad.Name())
					return nil
				}
				return err
			}
			fmt.Fprintln(os.Stderr, "aitori: CA installed into the OS trust store")
			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "remove",
		Short: "Remove the CA from the OS trust store",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ad := adapter.New()
			if err := ad.UninstallCA(); err != nil {
				if errors.Is(err, adapter.ErrNotImplemented) {
					fmt.Fprintf(os.Stderr, "aitori: automatic removal not available on %s yet; remove the certificate manually.\n", ad.Name())
					return nil
				}
				return err
			}
			fmt.Fprintln(os.Stderr, "aitori: CA removed from the OS trust store")
			return nil
		},
	}

	c.AddCommand(install, remove)
	return c
}

func cmdApps() *cobra.Command {
	return &cobra.Command{
		Use:   "apps",
		Short: "List configured app profiles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "APP\tATTRIBUTION\tMATCH")
			for i := range cfg.Apps {
				app := &cfg.Apps[i]
				attr, match := "process", appMatchSummary(&app.Match)
				if app.Match.Browser {
					attr = "host (browser)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", app.ID, attr, match)
			}
			return tw.Flush()
		},
	}
}

// appMatchSummary renders the dominant match field for the `apps` listing.
func appMatchSummary(m *config.AppMatch) string {
	switch {
	case m.BundleID != "":
		return m.BundleID
	case len(m.ExePaths) > 0:
		return m.ExePaths[0]
	case len(m.ProcessNames) > 0:
		return strings.Join(m.ProcessNames, ",")
	case len(m.Hosts) > 0:
		return strings.Join(m.Hosts, ",")
	default:
		return "-"
	}
}

func cmdConfig() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Configuration helpers"}
	validate := &cobra.Command{
		Use:   "validate [file]",
		Short: "Validate a config file (or the active config)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := flagConfig
			if len(args) == 1 {
				path = args[0]
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			where := path
			if where == "" {
				where = "(built-in defaults)"
			}
			fmt.Printf("OK: %s is valid (version %d, %d app profile(s))\n", where, cfg.Version, len(cfg.Apps))
			return nil
		},
	}
	c.AddCommand(validate)
	return c
}

func cmdStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show tier, gateway health, token state, and coverage",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			fmt.Printf("aitori %s\n", version.Version)
			fmt.Printf("platform:      %s\n", adapter.New().Name())
			fmt.Printf("transparent:   %t\n", cfg.Proxy.Transparent)
			fmt.Printf("listen:        %s\n", cfg.Proxy.Listen)
			fmt.Printf("CA cert:       %s\n", ca.CertPath(cfg.Proxy.CADir))

			// Token state.
			if cfg.Gateway.Auth.Disabled {
				fmt.Printf("token:         (auth disabled)\n")
			} else {
				tokState := token.StateNoToken
				if tok, err := token.NewFileSource(cfg.Gateway.Auth.TokenFile); err == nil {
					tokState = tok.State()
					tok.Close()
				}
				fmt.Printf("token:         %s (%s)\n", cfg.Gateway.Auth.TokenFile, tokState)
			}

			// Gateway reachability probe.
			if cfg.Gateway.URL == "" {
				fmt.Printf("gateway:       (not configured)\n")
			} else {
				fmt.Printf("gateway:       %s [%s]\n", cfg.Gateway.URL, probeGateway(cfg))
			}

			fmt.Printf("intercept:     %d host pattern(s), %d app profile(s)\n", len(cfg.InterceptHosts), len(cfg.Apps))
			return nil
		},
	}
}

func probeGateway(cfg *config.Config) string {
	u, err := cfg.GatewayURL()
	if err != nil || u == nil {
		return "invalid url"
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "443"
		if u.Scheme == "http" {
			port = "80"
		}
		host = net.JoinHostPort(host, port)
	}
	conn, err := net.DialTimeout("tcp", host, cfg.DialTimeout())
	if err != nil {
		return "unreachable"
	}
	conn.Close()
	return "reachable"
}
