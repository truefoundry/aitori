# Roadmap & status

Where aitori is and what's left, honestly. The OS-agnostic core (proxy,
reroute, classification, config, settings-mode injection) is implemented and
tested against a local gateway. The platform adapters cross-compile for all
three targets; what's been exercised on real hardware is the matrix below.

## Validated so far

✅ = verified end-to-end; — = not yet validated (works via config, not exercised).

| Platform | Claude Code (CLI) | Claude Desktop | Claude web | ChatGPT web |
|---|---|---|---|---|
| macOS | ✅ | ✅ | ✅ (Chrome) | ✅ |
| Windows | — | ✅ | ✅ | ✅ |
| Linux | ✅ (terminal) | — | — | — |

## Implemented

- **Selective-MITM proxy + reroute** (Tier 1, explicit `run` and system-proxy
  `up`), classification (`llm`/`mcp`/`other`), fail-open + circuit breaker,
  verbatim streaming, the lean three-header gateway contract (`x-tfy-api-key`,
  `x-tfy-original-url`, `x-tfy-metadata`).
- **OS adapters** for macOS / Linux / Windows: CA install, system proxy, per-OS
  attribution (PID via gopsutil on macOS/Linux; host-based on Windows).
- **Tier-0 settings injection** for clients that ignore the system proxy
  (e.g. Claude Code): patch the app's settings `env` with proxy + CA, applied on
  `up`, reverted on `down`.
- **Tier-2 transparent capture** on **Linux** (nftables `REDIRECT` +
  `SO_ORIGINAL_DST` + SNI dispatch) — experimental.
- Per-device CA (private key a `0600` file in `ca_dir`), pinning/CA-trust
  detection with actionable warnings, optional local observability sinks.

## Implemented but unverified on real hardware

Each needs a manual pass on a machine of that OS with admin rights:
- **Linux** beyond the validated Claude Code case: `gsettings` system proxy,
  NSS `certutil` (Chrome/Firefox), nftables transparent capture.
- **Windows**: `certutil -addstore Root`, WinINET registry proxy.

## Not yet implemented / planned

- **Per-app allow/block policy.** A request-level `block` action works (host/
  path/method-scoped → HTTP 403; see [configuration.md](configuration.md#blocking)),
  but there is no per-*app* allow/block yet — apps are tags, not gates. The goal is
  "allow the AI apps you sanction, block the ones you don't."
- **Cert-pinning → fail-closed/block.** A pinned allowlisted host fails open
  (ungoverned) today; add an option to deny instead.
- **macOS / Windows Tier-2 transparent capture** — native components outside
  this Go codebase (a signed macOS `NETransparentProxyProvider` sidecar; a
  Windows WFP callout). The Go side (`ServeTransparent`) is ready.
- **Code signing & notarization** — wired in `.goreleaser.yaml`, needs CI creds.
- **QUIC / HTTP-3** — a TCP proxy never sees UDP; block UDP/443 for intercepted
  hosts so clients fall back to TCP.
- **HTTP/2 on the MITM leg** — `force_http11` keeps both legs on HTTP/1.1.
- **`kill -9` self-revert** — a hard kill leaves OS state; mitigated by running
  under a supervisor that calls `aitori down` on stop.
- **App-profile verification** — every host/path rule in
  `internal/profiles/builtin.yaml` is a starting point; confirm against a real
  capture before relying on it.
