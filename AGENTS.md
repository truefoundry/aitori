# AGENTS.md

Orientation for contributors and coding agents working in this repo. User-facing
docs live in [README.md](README.md) and [docs/](docs/).

## Layout

```
cmd/aitori/            CLI entrypoint (cobra)
internal/
  config/               schema, load/validate, defaults, managed overlay
  hostmatch/            host-pattern matcher (exact, *.x, .suffix)
  token/                token-file reader + fsnotify watcher + cache
  ca/                   per-device CA + on-the-fly leaf certs
  classify/             rules engine + body heuristics (llm | mcp | other)
  circuit/              gateway circuit breaker
  router/               the reroute rewriter (x-tfy-* headers, dial redirect)
  appresolve/           flow -> app attribution (bundle/exe/process/host)
  pipeline/             resolve -> classify -> action (reroute | passthrough)
  proxy/                selective-MITM server, splice, streaming forward, fail-open
  adapter/              OS interface + build-tagged impls (darwin/linux/windows)
  clientcfg/            patches client config (Claude Code settings.json) on up/down
  sysuser/              SUDO_USER-aware home/uid resolution (single source of truth)
  inject/ sink/ capture/  Tier-0 injectors, observability sinks, exchange records
  profiles/             embedded built-in app profiles (//go:embed)
tools/aitori-gateway/  SEPARATE Go module: a local gateway with OTel->SQLite
                        tracing and a trace UI, for testing/demos
configs/                example + test configs
test/integration/       end-to-end tests (gateway + fake upstream + SSE)
```

## Multi-module repo + the CGO rule

Two Go modules:

- root (`github.com/truefoundry/aitori`) — the agent. Stays dependency-light.
- `tools/aitori-gateway` — its own module; carries the heavier OTel + SQLite
  deps so they never reach the agent.

**Always build and test with `CGO_ENABLED=0`.** A transitive gopsutil dependency
(`go-m1cpu`) crashes at init under cgo on recent Go/macOS. The `Makefile` sets
this for you; if you invoke `go` directly, set it yourself. The gateway uses the
pure-Go `modernc.org/sqlite` driver for the same reason.

## Build / test / lint

```bash
make build    # both modules -> ./bin
make test     # both modules
make vet fmt  # go vet + gofmt, both modules
make help     # all targets
```

`make test` covers the root module and `tools/aitori-gateway`. The race detector
needs cgo, so it can't run on the two gopsutil-importing adapter packages (see
[docs/development.md](docs/development.md)).

## Contracts to preserve

These are checked by tests in `internal/router`, `internal/proxy`, and
`test/integration`. Don't break them:

- The client-visible URL/Host never changes. Only the upstream TCP dial is
  redirected to the gateway; the original absolute URL rides in
  `x-tfy-original-url`.
- The app's own credentials (`Authorization` / `x-api-key` / `Cookie`) pass
  through untouched. `x-tfy-*` headers are additive, live only on the
  agent->gateway leg, and the gateway strips them before forwarding upstream.
- The gateway token is attached only on the reroute path — never logged, never in
  a URL, never on a passthrough/direct request.
- Fail-open by default: a gateway error forwards directly to the upstream and logs
  a governance gap. Configurable to fail-closed.
- Only `intercept_hosts` are decrypted; everything else is a raw splice.
- Responses stream unbuffered (SSE/chunked), with trailers preserved.

## Conventions

- `gofmt` everything; keep imports grouped (stdlib, third-party, local).
- OS-specific code goes behind the `adapter` interface in `adapter_<goos>.go`;
  everything else must compile on all three platforms.
- Resolve the invoking user's home via `internal/sysuser`, not `os.UserHomeDir`
  directly — under `sudo` they differ, and the CA path and client-config edits
  must agree.
- Built-in profile rules in `internal/profiles/builtin.yaml` are starting points;
  mark unverified host/path rules and confirm against a real capture.

## Why stdlib, not martian

The original plan proposed `github.com/google/martian`. The proxy core is instead
built directly on the standard library (`crypto/tls`, `crypto/x509`, `net/http`).
Martian MITMs every CONNECT (no native selective MITM), imposes a fixed
connection deadline that fights long-lived SSE, and pulls in gRPC/protobuf. A
focused stdlib implementation gives precise control over the correctness-critical
parts (selective MITM, verbatim streaming, dial redirection, fail-open) with a
small dependency footprint.

## More

- [docs/development.md](docs/development.md) — build from source, mock gateway, tests, release
- [docs/architecture.md](docs/architecture.md) — how aitori works in depth
- [docs/roadmap.md](docs/roadmap.md) — status, validated platforms, and open work
- [docs/configuration.md](docs/configuration.md) — config reference and CLI
