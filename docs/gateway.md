# Connecting to a real AI gateway

The bundled `aitori-gateway` (see [development.md](development.md)) is a
local stand-in for testing. To govern real traffic you point aitori at a real AI
gateway. The same [`configs/conversations.yaml`](../configs/conversations.yaml)
works — only the gateway endpoint and token change.

## The config overrides

Override the gateway endpoint and token; everything else (the intercept hosts,
app tags, inject recipes) stays the same. Then stop the mock gateway.

```yaml
gateway:
  url: https://your-gateway.example/api/llm/   # your gateway endpoint (path sent verbatim)
  on_error: fail_open                          # or fail_closed
  dial_timeout: 5s
  auth:
    token_file: ~/.aitori/token               # a file holding your gateway token
  headers:
    # Optional static headers added on the agent→gateway leg only. JSON values are
    # single-quoted so YAML keeps them strings, not nested maps.
    x-tfy-logging-config: '{"enabled": true}'
```

The same overrides are available as global CLI flags, which win over the config:

```bash
aitori run -c configs/conversations.yaml \
  --gateway-url https://your-gateway.example/api/llm/ \
  --token-file  ~/.aitori/token
```

Notes:

- A **path** on `gateway.url` (e.g. `/api/llm/tf-edge-proxy/`) is sent verbatim as
  the endpoint — the gateway routes to the real upstream from `x-tfy-original-url`,
  not from this path.
- `gateway.headers` adds static headers on the agent→gateway leg only. The three
  `x-tfy-*` identity headers win over any collision. The `x-tfy-logging-config`
  example above is one such static header some gateways read to toggle logging.
- The **token file** holds the bare gateway token (the secret sent as
  `x-tfy-api-key`). It's watched for changes and hot-reloads
  without a restart. If it's missing/empty, aitori fails open (and shows
  `no-token` in `aitori status`) unless `gateway.auth.disabled: true` (or
  `--no-auth`) is set, which reroutes without a token for gateways that
  authenticate the agent another way (mTLS, network ACL).

## Example: TrueFoundry AI Gateway

For the TrueFoundry AI Gateway, the exact steps — where to copy the base URL and
API key, the required `tf-edge-proxy/` path suffix, and where to save the token —
are in [truefoundry_gateway.md](truefoundry_gateway.md).

## The reroute contract (gateway side)

A gateway that aitori reroutes to must implement the following. The bundled
`tools/aitori-gateway` is a reference implementation of exactly this.

For each rerouted request, aitori sends:

- the original **method, path, query, and body**, verbatim;
- **all original headers**, including the app's own provider credentials
  (`Authorization`, `x-api-key`, `Cookie`, `anthropic-version`, …);
- `Host` set to the gateway host (aitori dials the gateway over normal verified
  TLS — no MITM on this leg);
- exactly three added `x-tfy-*` headers (plus any static `gateway.headers`):

  | Header | Carries |
  |---|---|
  | `x-tfy-api-key` | the gateway token (identity) |
  | `x-tfy-original-url` | the full original absolute URL `scheme://host[:port]/path?query` |
  | `x-tfy-metadata` | attribution JSON (all-string values, each ≤128 chars): `{app, pid, category, host, os, agent_version}` |

The gateway must:

1. **Authenticate** the user from `x-tfy-api-key`.
2. **Log** the request (asynchronously — don't block the proxied call on logging).
   Read per-request attribution from the `x-tfy-metadata` JSON.
3. **Strip every `x-tfy-*` header** before forwarding upstream, so the provider
   never sees them.
4. **Reset `Host`** to the original host and **forward to `x-tfy-original-url`**,
   preserving method/path/query/body and all remaining headers — including the
   original provider credentials.
5. **Stream the response back verbatim**: status code, all headers (including
   `Set-Cookie`), body, SSE/chunked framing, and trailers. No transformation, no
   buffering.
6. **Not auto-retry** non-idempotent methods.

Invariants that must hold: the provider authenticates the user via the user's own
credential (the gateway token never reaches the provider); response bytes returned
to the app are byte-identical to what the upstream produced; streaming cadence is
preserved; and for browser apps the gateway hostname is never exposed to the
browser (its entire view is the original host, so cookie scoping / CORS / CSP are
unaffected).

See [architecture.md](architecture.md) for how the agent side builds this leg.
</content>
