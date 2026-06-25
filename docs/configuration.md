# Configuration

aitori reads a single YAML file, or none at all. This page explains the model and
documents every key. [`configs/conversations.yaml`](../configs/conversations.yaml)
is the same thing as a worked, fully-commented example.

## Do you need a config file?

Often not. The built-in profiles already govern the common apps (Claude Code,
Claude Desktop, Claude web, ChatGPT, and the Anthropic and OpenAI APIs), so
`aitori up` works with no file. You write a config to add your own hosts, point
at a gateway, turn on the live UI, or change a default.

The smallest useful file watches the built-in traffic with no gateway, and shows
it live:

```yaml
version: 1
ui:
  enabled: true            # live-traffic view at http://127.0.0.1:9100
# no `gateway:` block, so nothing is rerouted: requests are inspected and passed through
```

Add a gateway and a host to start governing your own traffic:

```yaml
version: 1
gateway:
  url: https://gateway.example.com/api/llm
  auth:
    token_file: ~/.aitori/token
intercept_hosts:
  - api.example.com        # decrypt this host; its model/MCP calls reroute
```

## The model

A request goes through aitori in three steps. First, aitori decides whether to
decrypt it, based on its host. Second, if it was decrypted, aitori labels it with
the application that made it. Third, it chooses an action for the request, from a
matching rule, the host's own setting, or a guess based on the body. The action is
to reroute it through the gateway, pass it through unchanged, or block it.

The top-level keys map onto those steps:

| Key | What it controls |
|---|---|
| `intercept_hosts` | the hosts to decrypt; everything else is passed through untouched |
| `apps` | labels that attribute a decrypted request to an application |
| `rules` | optional per-path policy that overrides a host's default |
| `gateway` | where rerouted calls go, and how they authenticate |
| `proxy` | the local listener, the CA directory, and how bodies are handled |
| `ui` | the built-in live-traffic view |
| `sinks` | JSON-line logs of each exchange |
| `inject` | settings-patching for clients that ignore the system proxy |
| `builtin_profiles` | whether the bundled defaults are included (default: yes) |

The schema is strict: an unknown key is an error, so a typo fails at
`aitori config validate` rather than being ignored.

## `intercept_hosts` — what to decrypt

The hosts aitori is allowed to decrypt. A request to any host not listed here is
passed through without being decrypted. Each entry is either a host pattern or an
object.

A bare pattern can be exact (`api.anthropic.com`), a subdomain wildcard
(`*.anthropic.com`), or a suffix (`.anthropic.com`):

```yaml
intercept_hosts:
  - api.anthropic.com
  - "*.openai.com"
```

The object form attaches a policy to the host, and can narrow it to specific paths
and methods:

```yaml
intercept_hosts:
  - host: claude.ai
    paths: ["/api/organizations/*/chat_conversations/*/completion"]
    methods: ["POST"]
    category: llm
    action: reroute       # optional; a scoped entry defaults to reroute
```

In path patterns, `*` matches one path segment and `**` matches any number,
including slashes. When you set `paths` or `methods`, only the matching requests
get the policy; every other request on that host is passed through. A scoped entry
with no `action` defaults to `reroute`, since the reason to scope a host is usually
to trace those endpoints. A bare host (no paths) leaves the decision to the body
heuristic: model and MCP calls reroute, everything else passes through.

Do not list your gateway's host here. aitori rejects it, so the agent cannot end
up intercepting its own traffic to the gateway.

## `apps` — how requests are labelled

An app profile attaches a name to a decrypted request, so logs and the gateway can
tell where it came from. Apps are labels only; they do not decide what gets
decrypted (that is `intercept_hosts`).

```yaml
apps:
  - id: claude-code
    match: { exe_paths: ["~/.local/share/claude/versions"], process_names: ["claude"] }
  - id: chatgpt-web
    match: { browser: true, hosts: ["chatgpt.com"] }
```

Match a desktop or command-line app by `bundle_id` (macOS), `exe_paths`, or
`process_names`. Match browser traffic with `browser: true` and a list of `hosts`.

## `rules` and actions — policy

A request's action is `reroute`, `passthrough`, or `block`. It is chosen in this
order: a matching top-level rule wins first, then the host's own `action`, and
finally the body heuristic. `reroute` is the default for model and MCP calls.

`rules` are optional and let you set policy by path, independent of the host's
default or which app made the request:

```yaml
rules:
  - name: block-uploads
    hosts: ["api.openai.com"]
    path_prefixes: ["/v1/files"]
    methods: ["POST"]
    action: block
```

A rule matches on `hosts`, `path_prefixes`, `path_patterns`, `methods`, and
`match_body` (shallow JSON key/value checks), and sets an `action` and/or
`category`. See [Blocking](#blocking) for the `block` action.

## `gateway` — where rerouted calls go

```yaml
gateway:
  url: https://gateway.example.com/api/llm
  on_error: fail_open      # default; or fail_closed
  auth:
    token_file: ~/.aitori/token
```

- `url` — the gateway endpoint. Any path on it (for example `/api/llm/...`) is sent
  as-is. Leave `gateway` out entirely to run without one: reroute decisions then
  fail open, so requests are still decrypted, classified, and logged, then passed
  through to their real destination.
- `on_error` — what to do when the gateway is unreachable. `fail_open` (the
  default) sends the request to its real destination; `fail_closed` returns an
  error instead.
- `auth.token_file` — a file holding the bare gateway token. The token is the
  gateway's identity, which is why it lives under `gateway`.
- `auth.disabled: true` — reroute without requiring or sending a token, for
  gateways that authenticate the agent some other way (mTLS, a network ACL) or for
  local testing.
- `headers` — static headers added on the reroute leg only.

On a reroute, aitori adds three headers: the token (`header_token`, default
`x-tfy-api-key`), the original URL (`header_orig_url`, default
`x-tfy-original-url`), and a small JSON blob of attribution (`header_ctx`, default
`x-tfy-metadata`). Why only three, and what the gateway does with them, is in
[architecture.md](architecture.md#the-gateway-leg-a-lean-three-header-contract).

## `proxy` — listener, CA, and body handling

```yaml
proxy:
  listen: 127.0.0.1:8080
  ca_dir: ~/.aitori
```

- `listen` — the proxy's address (default `127.0.0.1:8080`).
- `ca_dir` — where the per-device CA is stored (default `~/.aitori`). Use a system
  path for a daemon or MDM deployment.
- `transparent: true` — opt into Tier-2 transparent capture on Linux (see
  [Capture tiers](architecture.md#capture-tiers)).
- `drain_timeout` — how long a graceful shutdown waits for in-flight requests
  before reverting OS state and exiting (default `5s`).
- `max_body_kb` — how much of a request body aitori will hold in memory (default
  `1024`). The body is read only when it can change the outcome: when a `match_body`
  rule exists, when no host/path/method rule matched (so the heuristic runs), or
  when the request reroutes (so it can be replayed if the gateway fails). A
  passthrough decided by host, path, or method streams its body straight through
  without buffering. Bodies larger than the cap always stream. Set `0` to disable
  buffering.
- `force_http11` — keep the connection on HTTP/1.1 (default `true`). Set `false` to
  allow HTTP/2; test SSE and your reroute and passthrough flows before relying on
  it.

## `ui` and `sinks` — watching traffic

The live UI is a self-contained page that shows each exchange as it happens — app,
category, action, host and path, status, and latency. It is off by default, holds
the last ~500 exchanges in memory (nothing is persisted), redacts query strings,
and is independent of the gateway.

```yaml
ui:
  enabled: true
  listen: 127.0.0.1:9100   # default
```

A sink writes one JSON line per exchange (never bodies) to stdout or a file.
Secrets are redacted by default; set `redact: false` to keep them.

```yaml
sinks:
  - type: stdout           # or: type: file, path: /var/log/aitori.jsonl
```

For a durable, searchable view, the bundled `aitori-gateway` records each call to
SQLite and serves its own trace UI; see [development.md](development.md).

## `inject` — clients that ignore the system proxy

Some clients (Claude Code, for example) ignore the system proxy and ship their own
CA bundle, so the system-proxy path alone will not capture them. For each enabled
`inject` entry, `aitori up` writes proxy and CA-trust variables into the client's
settings file, so a new session routes through aitori and trusts the device CA.
It is reverted on `down`. The only mode is `settings`:

```yaml
inject:
  - { app: claude-code, enabled: true }   # mode defaults to settings
```

aitori prefers the OS-managed settings path when it can write it (it can under
`up`, which runs as root or admin), and falls back to the per-user file:

| App | Managed (preferred) | Per-user fallback |
|---|---|---|
| Claude Code (macOS) | `/Library/Application Support/ClaudeCode/managed-settings.json` | `~/.claude/settings.json` |
| Claude Code (Linux) | `/etc/claude-code/managed-settings.json` | `~/.claude/settings.json` |
| Claude Code (Windows) | `%PROGRAMDATA%\ClaudeCode\managed-settings.json` | `~/.claude/settings.json` |

Built-in default paths live in `internal/clientcfg`. Override them per entry with
`settings: {managed_path, user_path}`.

## `builtin_profiles` — the bundled defaults

`true` by default. The built-in profiles
(`internal/profiles/builtin.yaml`) carry the same scoped Claude, ChatGPT,
Anthropic, and OpenAI endpoints as `configs/conversations.yaml`, and are merged
under your config, so a fresh `up` governs the common apps out of the box. Your
config wins on any conflict. Set `builtin_profiles: false` to govern only the
apps and hosts you list yourself.

## Blocking

`action: block` denies a decrypted request: aitori returns HTTP 403 with the body
`aitori: blocked by policy` and does not forward it. Set it on an `intercept_hosts`
entry or a rule, and scope it by path and method just like `reroute`:

```yaml
intercept_hosts:
  - { host: chatgpt.com, action: block }          # block the whole host

  - host: api.openai.com                           # or only some requests
    paths: ["/v1/chat/completions"]
    methods: ["POST"]
    action: block
```

Two limits to know. The host has to be decrypted (listed in `intercept_hosts`) and
the client has to trust the device CA; otherwise the TLS handshake fails before
aitori can return the 403, so the request is stopped, but as a connection error
rather than a clean 403. And blocking is per request, not per app: there is no
per-application allow/block policy yet, and a certificate-pinned host still fails
open rather than being blocked (see [roadmap.md](roadmap.md)).

## CLI

```
aitori run                 proxy only, no OS changes
aitori up                  install the CA, set the system proxy (or transparent), and run
aitori down                revert the proxy or transparent rules (also on Ctrl-C / SIGTERM)
aitori ca install|remove   manage the device CA in the OS trust store
aitori apps                list the app profiles
aitori config validate [file]
aitori status              tier, gateway health, token state, coverage
```

Global flags work on any command and override the loaded config:

```
-c, --config <path>     config file (otherwise built-in defaults plus profiles)
-v, --verbose           debug logging
    --gateway-url <url>  override gateway.url
    --header-ctx <name>  override gateway.header_ctx
    --token-file <path>  override gateway.auth.token_file
    --listen <addr>      override proxy.listen
    --ca-dir <path>      override proxy.ca_dir
    --transparent        enable transparent capture
    --no-auth            reroute without a token
    --ui                 serve the live-traffic UI
    --ui-listen <addr>   override ui.listen
```

`--transparent`, `--no-auth`, and `--ui` only ever turn a feature on; to force it
off, set the config key. The `Makefile` wraps the common commands (`make run`,
`make up`, `make down`, `make validate`), each taking `CONFIG=...`.

## Service lifecycle

aitori runs in the foreground, under an OS supervisor (a LaunchDaemon, systemd
unit, or Windows service) that keeps it alive.

- **Shutdown.** SIGINT, SIGTERM, and SIGHUP drain in-flight requests (up to
  `proxy.drain_timeout`) and then revert OS state: the system proxy or nftables
  rules are cleared and settings injects are undone. Reverting matters, because the
  system proxy points apps at aitori; a proxy left in place after aitori stops
  would break all traffic. SIGHUP is included so that closing the controlling
  terminal reverts cleanly.
- **Reload.** SIGUSR1 (Unix only) reloads the governance config (intercept set,
  rules, gateway, pipeline) without dropping connections; the listener and OS state
  are left alone. The token file is watched and reloads on its own. On Windows,
  restart to pick up config changes.
- **Startup.** `up` first reverts anything left behind by an earlier unclean exit
  before applying the current config, so state does not accumulate across restarts.

A `kill -9` cannot run the revert code, so run aitori under a supervisor that
calls `aitori down` on stop.

## What aitori can and can't govern

- **stdio MCP** does not cross the network, so a proxy cannot see it. Only remote
  MCP (SSE or streamable HTTP) is captured.
- **Certificate-pinned apps** cannot be decrypted. If the app has a base-URL or
  proxy setting you can use settings injection; otherwise it is out of reach.
- **Backend-proxied apps** such as claude.ai and chatgpt.com send the metered model
  call from their own servers. You can govern the call to that backend, but not the
  model call itself.
- **WebSocket upgrades** are passed through unless the gateway supports relaying
  them.
