# Development

For building aitori from source, contributing, and exercising the whole
pipeline locally against the bundled mock gateway. If you just want to *run*
aitori, see [getting-started.md](getting-started.md).

The mock gateway (`aitori-gateway`) stands in for a real AI gateway: it logs
every rerouted call, strips the `x-tfy-*` headers, forwards to the real upstream
with the app's own credentials (so apps keep working), and records each call as a
trace you can browse in a web UI.

## 0. Prerequisites

- **Go 1.25+** (`go version`).
- **macOS** for the full `up` experience — it installs a CA into the System
  keychain and sets the system proxy. The Linux/Windows adapters exist too;
  explicit-proxy mode works everywhere with no OS changes.
- **Admin rights** (`sudo`) only for `up` (CA install + system proxy).
- Builds use **`CGO_ENABLED=0`** (the `Makefile` sets it; a transitive `gopsutil`
  dependency crashes under cgo on recent Go/macOS, and aitori doesn't need it).

## 1. Build

```bash
git clone https://github.com/truefoundry/aitori
cd aitori
make build      # builds ./bin/aitori and ./bin/aitori-gateway (both modules)
```

## 2. Provide a token

The agent reads a gateway token from a file. Any non-empty value works for the
mock gateway:

```bash
mkdir -p ~/.aitori
echo "demo-token" > ~/.aitori/token
```

## 3. Start the mock gateway (Terminal 1)

```bash
make run-gateway          # or: ./bin/aitori-gateway -debug
# proxy:    http://127.0.0.1:9000
# trace UI: http://127.0.0.1:9000/ui/
```

It records each call as an OpenTelemetry span in a local SQLite file and serves a
trace UI at `http://127.0.0.1:9000/ui/`. `-debug` also logs request/response
bodies (gzip-decoded, text only) to stderr. By default it accepts reroutes
without a token (so aitori's `gateway.auth.disabled` / `--no-auth` works against
it); pass `-require-auth` to make it 401 on a missing `x-tfy-api-key` instead.

## 4. Run aitori

You have two ways to put aitori in the path.

### Explicit-proxy mode — no OS changes (Terminal 2)

You point clients at the proxy yourself. This generates the device CA at
`~/.aitori/ca.pem` and makes no system changes:

```bash
make run CONFIG=configs/conversations.yaml      # 127.0.0.1:8080
```

### `up` — govern the whole machine (macOS, needs sudo)

Installs the per-device CA into the System keychain, sets the system HTTP(S)
proxy to `127.0.0.1:8080`, and writes proxy + CA env vars into
`~/.claude/settings.json` so **Claude Code** is governed too (Node clients ignore
the system proxy and trust store otherwise). Everything is reverted on exit
(Ctrl-C / SIGTERM) and by `make down`:

```bash
make up   CONFIG=configs/conversations.yaml
# ... use your apps ...
make down                  # or just Ctrl-C the up process
```

Add `-v` for debug logs (connection open/close + durations, cert-pinning notices)
by invoking the binary directly: `sudo ./bin/aitori up -v -c configs/conversations.yaml`.

## 5. The httpbin smoke test

This exercises the whole pipeline against `httpbin.org` so you can see the headers
the gateway adds and strips, without needing a real AI app. Use a throwaway config
that intercepts only `httpbin.org`:

```bash
cat > /tmp/aitori-httpbin.yaml <<'YAML'
version: 1
proxy:
  listen: 127.0.0.1:8080
  ca_dir: ~/.aitori
  force_http11: true
gateway:
  url: http://127.0.0.1:9000
  on_error: fail_open
  auth:
    token_file: ~/.aitori/token
intercept_hosts:
  - host: httpbin.org
    paths: ["/post"]   # POST /post reroutes; everything else passes through
    methods: ["POST"]
    category: llm
    action: reroute
YAML
```

With the gateway running (step 3) and aitori on this config
(`./bin/aitori run -c /tmp/aitori-httpbin.yaml`), send a request through the
proxy. `--cacert` trusts the device CA so the MITM leg validates:

```bash
curl -sx http://127.0.0.1:8080 --cacert ~/.aitori/ca.pem \
  https://httpbin.org/post \
  -H 'authorization: Bearer my-provider-key' \
  -H 'content-type: application/json' \
  -d '{"model":"x","messages":[]}' | jq .headers
```

What to look for:

- **Reroute** — Terminal 1 logs the call and shows it received `x-tfy-api-key`,
  `x-tfy-original-url` (`POST https://httpbin.org/post`), and `x-tfy-metadata`.
  httpbin echoes the headers the *upstream* saw: `Authorization` is present
  (preserved) and there are no `X-Tfy-*` headers (the gateway stripped them).
- **Passthrough** — a non-matching request never hits the gateway or carries any
  `x-tfy-*`:

  ```bash
  curl -sx http://127.0.0.1:8080 --cacert ~/.aitori/ca.pem \
    https://httpbin.org/get | jq .headers
  ```

- **Fail-open** — stop the gateway (Ctrl-C in Terminal 1) and re-run the `/post`
  request: it still succeeds (forwarded straight to httpbin) and aitori logs a
  "gateway error: governance gap" warning.
- **Non-allowlisted host** — any host not in `intercept_hosts` is tunneled raw, so
  you don't even need `--cacert`:

  ```bash
  curl -sx http://127.0.0.1:8080 https://example.com/ -o /dev/null -w '%{http_code}\n'
  ```

## 6. Real apps

With `configs/conversations.yaml` and `up` (macOS), the bundled Claude profiles
are governed:

- **Claude Desktop** — send a message → Terminal 1 shows
  `reroute: POST https://claude.ai/... app="claude-desktop"`.
- **Claude web** (browser, claude.ai) → `app="claude-web"`.
- **Claude Code** — launch a **fresh** `claude` session (it reads the injected
  `~/.claude/settings.json` on start) →
  `reroute: POST https://api.anthropic.com/v1/messages ... app="claude-code"`.

In a browser without `up`: point the browser's HTTPS proxy at `127.0.0.1:8080`,
import `~/.aitori/ca.pem` into the browser/OS trust store, and add the target
hosts to `intercept_hosts`.

## 7. Teardown

```bash
make down                          # reverts system proxy AND ~/.claude/settings.json
sudo ./bin/aitori ca remove       # remove the CA from the System keychain
```

The CA is intentionally left installed by a normal stop; only `ca remove` deletes
it.

## Useful commands

- `./bin/aitori config validate <file>` — check a config without running.
- `./bin/aitori apps -c <file>` — list the governed app profiles.
- `./bin/aitori status` — tier, gateway health, token state, coverage.

## Automated tests

```bash
make test    # unit + integration, both modules (CGO off)
make vet     # go vet, both modules
```

The integration suite (`test/integration`) drives real requests through the proxy
against a local gateway and a fake upstream and checks the whole contract: correct
`x-tfy-*` headers on the gateway leg, provider auth preserved, no `x-tfy-*` leaking
upstream, byte-identical SSE streaming, non-allowlisted hosts never decrypted, and
fail-open when the gateway dies. The race detector (`go test -race`) forces cgo on,
which trips a `go-m1cpu` init crash for the two `gopsutil`-importing adapter
packages — run `-race` on the other packages if you need it.

## Releasing

Cutting a release (goreleaser tarballs, the `install.sh` flow) is documented in
[release.md](release.md). The conventions and contracts to preserve are in
[../AGENTS.md](../AGENTS.md).
