# Capturing browser AI traffic — an extension to the Claude Code MDM setup

> This guide extends
> [Securing Claude Code with TrueFoundry](https://www.truefoundry.com/docs/ai-gateway/mcp/enterprise-security-claude).
> That guide governs the Claude Code CLI. This one adds capture of **browser** AI
> traffic — **Claude web (claude.ai)** and **ChatGPT web (chatgpt.com)** — by
> deploying [aitori](https://github.com/truefoundry/aitori) as a background service
> on macOS and Linux. It is a **separate add-on script** you schedule alongside the
> one you already run; it does not replace it.

## Why a second mechanism is needed

The Claude Code setup works because the CLI reads its configuration from a file you
control: you point `ANTHROPIC_BASE_URL` at the gateway and Claude Code obeys. A web
browser offers no such setting — when a developer opens claude.ai, the browser
negotiates TLS directly with Anthropic and sends the conversation there, with no
knob that redirects it to a gateway.

aitori intervenes on the machine itself. It runs as a local proxy, installs a
certificate the machine trusts, and for a small, explicit list of hostnames it
decrypts the TLS connection, inspects the request, and forwards model calls to the
TrueFoundry AI Gateway, which logs them and relays them to the real provider. The
browser receives a byte-identical response. Every host not on the list is passed
through untouched, never decrypted.

Two properties matter for an enterprise rollout. The list of intercepted hosts is
**explicit and auditable** — aitori decrypts what you name and nothing else. And the
proxy is **fail-open**: if the gateway or credential is unavailable, the request
still reaches its original destination. Governance degrades to transparency, not to
a broken browser.

## What this adds, and what it does not touch

| Captured | Not touched |
|---|---|
| **Claude web** — conversation calls to `claude.ai` | The Claude Code CLI (governed by the base setup) |
| **ChatGPT web** — conversation calls to `chatgpt.com` | Claude Desktop |
| | The Anthropic / OpenAI **APIs** (`api.anthropic.com`, `api.openai.com`) |

The deployed config sets `builtin_profiles: false` and lists exactly two hosts. This
guarantees no Claude Code settings file is read or written by this add-on, and the
decrypt list contains only claude.ai and chatgpt.com. Confirm it at any time:

```bash
aitori status -c <config-path>
#   gateway:    https://<host>/api/llm/ai-proxy/ [reachable]
#   token:      .../tf_token (ok)
#   intercept:  2 host pattern(s), 2 app profile(s)
```

More than two host patterns means the config has drifted from this guide.

## Prerequisites

1. **The Claude Code MDM script is already deployed.** Its first run performs the
   browser device-login and caches a refresh token at `~/.tfy-refresh-token`. The
   add-on reuses that token — it does not run its own login.
2. **The shared config values are on disk.** The base script saves
   `CONTROL_PLANE_URL`, `TENANT_NAME`, and `GATEWAY_URL` to a root-owned conf file
   (macOS `/Library/Preferences/com.truefoundry.tfy-local-ai-setup.conf`, Linux
   `/etc/tfy/tfy-local-ai-setup.conf`). The add-on reads these; you do not re-enter
   them.
3. **Outbound access** to GitHub Releases (binary download) and to your control
   plane (token exchange).

The add-on exchanges the cached refresh token for a short-lived gateway access token
and writes it to the file aitori reads (`gateway.auth.token_file`); aitori watches
that file and reloads a new token without restarting. The exchange **rotates** the
refresh token, so the script saves the rotated value back. Do not test the exchange
with an ad-hoc `curl` — each call invalidates the previous refresh token.

## Listen port

aitori listens on a local TCP port, configured by `AITORI_LISTEN` and defaulting to
`127.0.0.1:8123`. Avoid `8080`/`8081` (used by the base Claude Code sandbox) and
other common ports (`3000`, `8000`, `8888`, `9090`). On Linux, transparent mode
redirects traffic to this same port, so the script derives both from the one
variable.

## How aitori is kept running

The scripts register aitori with the platform's service manager so it starts at
boot, restarts on crash, and reverts cleanly on stop (a clean stop sends SIGTERM,
which makes aitori undo the system proxy / redirect rules — so it never strands the
machine's traffic).

| Property | macOS (launchd) | Linux (systemd) |
|---|---|---|
| Start at boot | `RunAtLoad` | `WantedBy=multi-user.target` + `enable` |
| Restart on crash | `KeepAlive` | `Restart=always`, `RestartSec=1` |
| Clean revert on stop | SIGTERM on `bootout` | `KillSignal=SIGTERM` |
| Idempotent re-run | `bootout` then `bootstrap` | `daemon-reload` + `enable --now` |

---

## macOS

Run as **root** from your MDM (Jamf, Mosyle, Kandji), hourly, offset by thirty
minutes from the base script (see [Scheduling](#scheduling)). The certificate goes
into the System keychain and the proxy is set with `networksetup` — both machine-wide
and headless, so Safari and Chrome are covered with no per-user step. The script is
idempotent.

```bash
#!/bin/bash
# MDM add-on: aitori browser AI capture — macOS (arm64 + amd64)
# Extends the Claude Code MDM setup. Captures Claude web + ChatGPT web.
# Does NOT touch Claude Code. Must run as root, offset from the base script.

set -euo pipefail
[[ "$(id -u)" -ne 0 ]] && { echo "ERROR: must run as root." >&2; exit 1; }

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] aitori-addon: $*"; }

# --- Settings specific to this add-on ---------------------------------------
AITORI_LISTEN="127.0.0.1:8123"     # uncommon port; avoid 8080/3000/8000/8888/9090
RELEASE_TAG="v0.1.0"               # aitori release to pin
AITORI_REPO="truefoundry/aitori"

BASE_CONF="/Library/Preferences/com.truefoundry.tfy-local-ai-setup.conf"
APP_DIR="/Library/Application Support/aitori"
CONFIG_FILE="${APP_DIR}/config.yaml"
TOKEN_FILE="${APP_DIR}/tf_token"
BINARY_PATH="/usr/local/bin/aitori"
VERSION_FILE="${BINARY_PATH}.version"
PLIST="/Library/LaunchDaemons/com.truefoundry.aitori.plist"
LABEL="com.truefoundry.aitori"

# --- Reuse CONTROL_PLANE_URL / TENANT_NAME / GATEWAY_URL from the base script --
[[ -f "${BASE_CONF}" ]] || { log "ERROR: ${BASE_CONF} not found — deploy the Claude Code MDM script first."; exit 1; }
CONTROL_PLANE_URL=""; TENANT_NAME=""; GATEWAY_URL=""
while IFS='=' read -r k v; do
  [[ -z "${k}" || "${k}" == \#* ]] && continue
  v="${v%\"}"; v="${v#\"}"
  case "${k// /}" in
    CONTROL_PLANE_URL) CONTROL_PLANE_URL="${v}" ;;
    TENANT_NAME)       TENANT_NAME="${v}" ;;
    GATEWAY_URL)       GATEWAY_URL="${v}" ;;
  esac
done < "${BASE_CONF}"
[[ -n "${CONTROL_PLANE_URL}" && -n "${TENANT_NAME}" && -n "${GATEWAY_URL}" ]] \
  || { log "ERROR: CONTROL_PLANE_URL / TENANT_NAME / GATEWAY_URL missing from ${BASE_CONF}."; exit 1; }

AITORI_GATEWAY_URL="${GATEWAY_URL%/}/ai-proxy/"   # aitori targets the edge-proxy route

# Logged-in user (their home holds ~/.tfy-refresh-token).
USER_NAME="$(/usr/sbin/scutil <<< 'show State:/Users/ConsoleUser' | awk '/Name :/ {print $3}')"
[[ -n "${USER_NAME}" && "${USER_NAME}" != "loginwindow" ]] || USER_NAME=""

# --- Exchange refresh token -> access token (as the user) -------------------
# Writes the access token to aitori's token file and saves the rotated refresh
# token back so the base script's next run still finds a valid one.
mkdir -p "${APP_DIR}"
if [[ -n "${USER_NAME}" ]]; then
  USER_HOME="$(/usr/bin/dscl . -read "/Users/${USER_NAME}" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  RT_FILE="${USER_HOME}/.tfy-refresh-token"
  if [[ -s "${RT_FILE}" ]]; then
    RESP="$(sudo -u "${USER_NAME}" bash -c '
      rt="$(cat "'"${RT_FILE}"'")"
      curl -fsS -X POST "'"${CONTROL_PLANE_URL}"'/api/svc/v1/oauth2/token" \
        -H "Content-Type: application/json" \
        -d "{\"grantType\":\"refresh_token\",\"tenantName\":\"'"${TENANT_NAME}"'\",\"refreshToken\":\"${rt}\",\"returnJWT\":true}"
    ' 2>/dev/null || true)"
    if command -v jq >/dev/null 2>&1; then
      ACCESS_TOKEN="$(printf '%s' "${RESP}" | jq -r '.accessToken // empty')"
      NEW_RT="$(printf '%s' "${RESP}" | jq -r '.refreshToken // empty')"
    else
      ACCESS_TOKEN="$(printf '%s' "${RESP}" | grep -o '"accessToken":"[^"]*"'  | sed 's/.*:"//;s/"$//')"
      NEW_RT="$(printf '%s' "${RESP}"      | grep -o '"refreshToken":"[^"]*"' | sed 's/.*:"//;s/"$//')"
    fi
    [[ -n "${NEW_RT}" ]] && { printf '%s' "${NEW_RT}" | sudo -u "${USER_NAME}" tee "${RT_FILE}" >/dev/null; sudo -u "${USER_NAME}" chmod 600 "${RT_FILE}"; }
    if [[ -n "${ACCESS_TOKEN}" ]]; then
      printf '%s' "${ACCESS_TOKEN}" > "${TOKEN_FILE}"; chmod 600 "${TOKEN_FILE}"
      log "aitori gateway token refreshed"
    else
      log "WARNING: token exchange returned no accessToken — keeping previous token; aitori fail-opens until next run"
    fi
  else
    log "NOTE: ${RT_FILE} not found — run the Claude Code device login first; aitori fail-opens meanwhile"
  fi
fi

# --- Install / update the aitori binary (skip if already on RELEASE_TAG) -----
INSTALLED_TAG="$([[ -f "${VERSION_FILE}" ]] && cat "${VERSION_FILE}" || echo '')"
if [[ ! -f "${BINARY_PATH}" || "${INSTALLED_TAG}" != "${RELEASE_TAG}" ]]; then
  case "$(uname -m)" in
    arm64)  arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) log "ERROR: unsupported arch $(uname -m)"; exit 1 ;;
  esac
  ver="${RELEASE_TAG#v}"
  asset="aitori_${ver}_darwin_${arch}.tar.gz"
  base="https://github.com/${AITORI_REPO}/releases/download/${RELEASE_TAG}"
  tmp="$(mktemp -d)"; trap 'rm -rf "${tmp}"' EXIT
  log "downloading ${asset} (${RELEASE_TAG})"
  curl -fsSL "${base}/${asset}"      -o "${tmp}/${asset}"
  curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt"
  want="$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}')"
  got="$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')"
  [[ "${want}" == "${got}" ]] || { log "ERROR: checksum mismatch for ${asset}"; exit 1; }
  tar -xzf "${tmp}/${asset}" -C "${tmp}"
  install -m 0755 "${tmp}/aitori" "${BINARY_PATH}"
  echo "${RELEASE_TAG}" > "${VERSION_FILE}"
  log "installed aitori ${RELEASE_TAG}"
fi

# --- Write the aitori config (absolute paths: a daemon has no SUDO_USER) -----
cat > "${CONFIG_FILE}" <<YAML
version: 1
builtin_profiles: false          # decrypt ONLY the hosts below; no Claude Code inject, no APIs
proxy:
  listen: ${AITORI_LISTEN}
  ca_dir: ${APP_DIR}
gateway:
  url: ${AITORI_GATEWAY_URL}
  on_error: fail_open
  auth:
    token_file: ${TOKEN_FILE}
intercept_hosts:
  - host: claude.ai
    paths:
      - "/api/organizations/*/chat_conversations/*/completion"
      - "/api/organizations/*/chat_conversations/*/retry_completion"
      - "/api/append_message"
    methods: ["POST"]
    category: llm
    action: reroute
  - host: chatgpt.com
    paths:
      - "/backend-anon/f/conversation"
      - "/backend-api/conversation"
      - "/backend-api/f/conversation"
    methods: ["POST"]
    category: llm
    action: reroute
apps:
  - { id: claude-web,  match: { browser: true, hosts: ["claude.ai"] } }
  - { id: chatgpt-web, match: { browser: true, hosts: ["chatgpt.com"] } }
YAML
chmod 644 "${CONFIG_FILE}"

# --- Register the launchd daemon (start at boot + restart on crash) ----------
cat > "${PLIST}" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${BINARY_PATH}</string>
    <string>up</string>
    <string>-c</string>
    <string>${CONFIG_FILE}</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/var/log/aitori.log</string>
  <key>StandardErrorPath</key><string>/var/log/aitori.log</string>
</dict>
</plist>
PLIST
chmod 644 "${PLIST}"

launchctl bootout system "${PLIST}" 2>/dev/null || true   # idempotent reload
launchctl bootstrap system "${PLIST}"
launchctl enable "system/${LABEL}"
log "aitori service loaded on ${AITORI_LISTEN}"
```

---

## Linux

Run as **root** from your Linux MDM or config-management tool, hourly, offset from
the base script. Two differences from macOS, both because Linux has no machine-wide
proxy or trust store:

- **Capture** uses aitori's **transparent mode** (`nftables` redirect of outbound
  80/443), which works under a root daemon where the GNOME proxy would not. The
  redirect targets `AITORI_LISTEN`.
- **Trust:** browsers use their own NSS store in the user's home, which a root
  daemon cannot reach. After the service starts, the script trusts the certificate
  in the logged-in user's NSS store with `certutil` (covers Chrome and Firefox).
  Requires `libnss3-tools` (Debian/Ubuntu) or `nss-tools` (RHEL/Fedora).

```bash
#!/bin/bash
# MDM add-on: aitori browser AI capture — Linux (amd64/arm64, systemd)
# Extends the Claude Code MDM setup. Captures Claude web + ChatGPT web.
# Does NOT touch Claude Code. Must run as root, offset from the base script.

set -euo pipefail
[[ "$(id -u)" -ne 0 ]] && { echo "ERROR: must run as root." >&2; exit 1; }

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] aitori-addon: $*"; }

# --- Settings specific to this add-on ---------------------------------------
AITORI_LISTEN="127.0.0.1:8123"     # uncommon port; avoid 8080/3000/8000/8888/9090
RELEASE_TAG="v0.1.0"
AITORI_REPO="truefoundry/aitori"

BASE_CONF="/etc/tfy/tfy-local-ai-setup.conf"
APP_DIR="/etc/aitori"
CONFIG_FILE="${APP_DIR}/config.yaml"
TOKEN_FILE="${APP_DIR}/tf_token"
CA_FILE="${APP_DIR}/ca.pem"
BINARY_PATH="/usr/local/bin/aitori"
VERSION_FILE="${BINARY_PATH}.version"
UNIT="/etc/systemd/system/aitori.service"

# --- Reuse CONTROL_PLANE_URL / TENANT_NAME / GATEWAY_URL from the base script --
[[ -f "${BASE_CONF}" ]] || { log "ERROR: ${BASE_CONF} not found — deploy the Claude Code MDM script first."; exit 1; }
CONTROL_PLANE_URL=""; TENANT_NAME=""; GATEWAY_URL=""
while IFS='=' read -r k v; do
  [[ -z "${k}" || "${k}" == \#* ]] && continue
  v="${v%\"}"; v="${v#\"}"
  case "${k// /}" in
    CONTROL_PLANE_URL) CONTROL_PLANE_URL="${v}" ;;
    TENANT_NAME)       TENANT_NAME="${v}" ;;
    GATEWAY_URL)       GATEWAY_URL="${v}" ;;
  esac
done < "${BASE_CONF}"
[[ -n "${CONTROL_PLANE_URL}" && -n "${TENANT_NAME}" && -n "${GATEWAY_URL}" ]] \
  || { log "ERROR: CONTROL_PLANE_URL / TENANT_NAME / GATEWAY_URL missing from ${BASE_CONF}."; exit 1; }

AITORI_GATEWAY_URL="${GATEWAY_URL%/}/ai-proxy/"   # aitori targets the edge-proxy route
USER_NAME="${SUDO_USER:-$(logname 2>/dev/null || true)}"

# --- Exchange refresh token -> access token (as the user) -------------------
# Writes the access token to aitori's token file and saves the rotated refresh
# token back so the base script's next run still finds a valid one.
mkdir -p "${APP_DIR}"
if [[ -n "${USER_NAME}" && "${USER_NAME}" != "root" ]]; then
  USER_HOME="$(getent passwd "${USER_NAME}" | cut -d: -f6)"
  RT_FILE="${USER_HOME}/.tfy-refresh-token"
  if [[ -s "${RT_FILE}" ]]; then
    RESP="$(sudo -u "${USER_NAME}" bash -c '
      rt="$(cat "'"${RT_FILE}"'")"
      curl -fsS -X POST "'"${CONTROL_PLANE_URL}"'/api/svc/v1/oauth2/token" \
        -H "Content-Type: application/json" \
        -d "{\"grantType\":\"refresh_token\",\"tenantName\":\"'"${TENANT_NAME}"'\",\"refreshToken\":\"${rt}\",\"returnJWT\":true}"
    ' 2>/dev/null || true)"
    if command -v jq >/dev/null 2>&1; then
      ACCESS_TOKEN="$(printf '%s' "${RESP}" | jq -r '.accessToken // empty')"
      NEW_RT="$(printf '%s' "${RESP}" | jq -r '.refreshToken // empty')"
    else
      ACCESS_TOKEN="$(printf '%s' "${RESP}" | grep -o '"accessToken":"[^"]*"'  | sed 's/.*:"//;s/"$//')"
      NEW_RT="$(printf '%s' "${RESP}"      | grep -o '"refreshToken":"[^"]*"' | sed 's/.*:"//;s/"$//')"
    fi
    [[ -n "${NEW_RT}" ]] && { printf '%s' "${NEW_RT}" | sudo -u "${USER_NAME}" tee "${RT_FILE}" >/dev/null; sudo -u "${USER_NAME}" chmod 600 "${RT_FILE}"; }
    if [[ -n "${ACCESS_TOKEN}" ]]; then
      printf '%s' "${ACCESS_TOKEN}" > "${TOKEN_FILE}"; chmod 600 "${TOKEN_FILE}"
      log "aitori gateway token refreshed"
    else
      log "WARNING: token exchange returned no accessToken — keeping previous token; aitori fail-opens until next run"
    fi
  else
    log "NOTE: ${RT_FILE} not found — run the Claude Code device login first; aitori fail-opens meanwhile"
  fi
fi

# --- Install / update the aitori binary -------------------------------------
INSTALLED_TAG="$([[ -f "${VERSION_FILE}" ]] && cat "${VERSION_FILE}" || echo '')"
if [[ ! -f "${BINARY_PATH}" || "${INSTALLED_TAG}" != "${RELEASE_TAG}" ]]; then
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) log "ERROR: unsupported arch $(uname -m)"; exit 1 ;;
  esac
  ver="${RELEASE_TAG#v}"
  asset="aitori_${ver}_linux_${arch}.tar.gz"
  base="https://github.com/${AITORI_REPO}/releases/download/${RELEASE_TAG}"
  tmp="$(mktemp -d)"; trap 'rm -rf "${tmp}"' EXIT
  log "downloading ${asset} (${RELEASE_TAG})"
  curl -fsSL "${base}/${asset}"      -o "${tmp}/${asset}"
  curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt"
  want="$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}')"
  got="$(sha256sum "${tmp}/${asset}" | awk '{print $1}')"
  [[ "${want}" == "${got}" ]] || { log "ERROR: checksum mismatch for ${asset}"; exit 1; }
  tar -xzf "${tmp}/${asset}" -C "${tmp}"
  install -m 0755 "${tmp}/aitori" "${BINARY_PATH}"
  echo "${RELEASE_TAG}" > "${VERSION_FILE}"
  log "installed aitori ${RELEASE_TAG}"
fi

# --- Write the aitori config (transparent: true → nftables capture) ----------
cat > "${CONFIG_FILE}" <<YAML
version: 1
builtin_profiles: false          # decrypt ONLY the hosts below; no Claude Code inject, no APIs
proxy:
  listen: ${AITORI_LISTEN}
  ca_dir: ${APP_DIR}
  transparent: true
gateway:
  url: ${AITORI_GATEWAY_URL}
  on_error: fail_open
  auth:
    token_file: ${TOKEN_FILE}
intercept_hosts:
  - host: claude.ai
    paths:
      - "/api/organizations/*/chat_conversations/*/completion"
      - "/api/organizations/*/chat_conversations/*/retry_completion"
      - "/api/append_message"
    methods: ["POST"]
    category: llm
    action: reroute
  - host: chatgpt.com
    paths:
      - "/backend-anon/f/conversation"
      - "/backend-api/conversation"
      - "/backend-api/f/conversation"
    methods: ["POST"]
    category: llm
    action: reroute
apps:
  - { id: claude-web,  match: { browser: true, hosts: ["claude.ai"] } }
  - { id: chatgpt-web, match: { browser: true, hosts: ["chatgpt.com"] } }
YAML
chmod 644 "${CONFIG_FILE}"

# --- Register the systemd unit (start at boot + restart; SIGTERM revert) -----
cat > "${UNIT}" <<UNITFILE
[Unit]
Description=aitori — browser AI traffic capture
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=${BINARY_PATH} up -c ${CONFIG_FILE}
Restart=always
RestartSec=1
KillSignal=SIGTERM
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
UNITFILE

systemctl daemon-reload
systemctl enable --now aitori
log "aitori service enabled + started on ${AITORI_LISTEN}"

# --- Trust the CA in the logged-in user's NSS store (Chrome + Firefox) -------
for _ in $(seq 1 10); do [[ -f "${CA_FILE}" ]] && break; sleep 1; done   # wait for first-start CA
if [[ -n "${USER_NAME}" && "${USER_NAME}" != "root" && -f "${CA_FILE}" ]] && command -v certutil >/dev/null 2>&1; then
  USER_HOME="$(getent passwd "${USER_NAME}" | cut -d: -f6)"
  sudo -u "${USER_NAME}" mkdir -p "${USER_HOME}/.pki/nssdb"
  sudo -u "${USER_NAME}" certutil -N --empty-password -d "sql:${USER_HOME}/.pki/nssdb" 2>/dev/null || true
  for db in "${USER_HOME}"/.mozilla/firefox/*/ "${USER_HOME}/.pki/nssdb"; do
    [[ -d "${db}" ]] || continue
    sudo -u "${USER_NAME}" certutil -A -d "sql:${db}" -t "C,," -n aitori -i "${CA_FILE}" 2>/dev/null || true
  done
  log "trusted aitori CA in user NSS store"
else
  log "NOTE: skipped per-user NSS trust (no user, no CA yet, or certutil missing)."
fi

# Fleet alternative — Chrome/Edge enterprise policy (system-wide, survives new
# profiles; does NOT cover Firefox). Uncomment to use instead of / alongside NSS:
# mkdir -p /etc/opt/chrome/policies/certificates /etc/opt/edge/policies/certificates
# openssl x509 -in "${CA_FILE}" -outform der -out /etc/opt/chrome/policies/certificates/aitori.der
# cp /etc/opt/chrome/policies/certificates/aitori.der /etc/opt/edge/policies/certificates/aitori.der
```

The NSS step trusts the certificate for the one logged-in user, in the profiles that
exist when the script runs; the hourly cadence picks up new Firefox profiles on a
later run. It affects trust only — transparent mode redirects traffic regardless.

---

## Scheduling

Deploy this add-on through your MDM on the same hourly cadence as the base Claude
Code script, but **offset the two by thirty minutes** (e.g. base at `:00`, add-on at
`:30`). Both refresh the same single-use, rotating refresh token; the offset keeps
the two exchanges from running concurrently and invalidating each other.

## Verify

```bash
aitori status -c /Library/Application\ Support/aitori/config.yaml   # macOS
aitori status -c /etc/aitori/config.yaml                            # Linux
```

Expect the gateway reachable, the token `ok`, and `intercept: 2 host pattern(s)`.
Open claude.ai and chatgpt.com, send a message in each, and confirm the calls appear
in the TrueFoundry AI Gateway request logs.

Confirm the service: macOS `sudo launchctl list | grep com.truefoundry.aitori` (and
it survives reboot + `kill -9`); Linux `systemctl status aitori` active,
`nft list table inet aitori` present.

Confirm nothing else changed: no Claude Code `managed-settings.json` or
`~/.claude/settings.json` is touched, and `aitori status` lists only claude.ai and
chatgpt.com.

## Uninstall

Revert system changes while the binary still exists, then remove it.

**macOS**
```bash
sudo launchctl bootout system /Library/LaunchDaemons/com.truefoundry.aitori.plist
sudo aitori down -c "/Library/Application Support/aitori/config.yaml"   # clears the system proxy
sudo aitori ca remove                                                   # removes the device CA
sudo rm -f /Library/LaunchDaemons/com.truefoundry.aitori.plist /usr/local/bin/aitori*
sudo rm -rf "/Library/Application Support/aitori"
```

**Linux**
```bash
sudo systemctl disable --now aitori     # SIGTERM → aitori removes the nftables rules
sudo aitori ca remove
USER_HOME="$(getent passwd "$(logname)" | cut -d: -f6)"
for db in "$USER_HOME"/.mozilla/firefox/*/ "$USER_HOME/.pki/nssdb"; do
  [ -d "$db" ] && sudo -u "$(logname)" certutil -D -d "sql:$db" -n aitori 2>/dev/null || true
done
sudo rm -f /etc/systemd/system/aitori.service /usr/local/bin/aitori*
sudo rm -rf /etc/aitori
sudo systemctl daemon-reload
```

Leave `~/.tfy-refresh-token` in place — it belongs to the base Claude Code setup.
