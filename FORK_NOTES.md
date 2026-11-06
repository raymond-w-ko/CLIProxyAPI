# Fork differences

This file describes the current changes from upstream. Keep upstream release
history in Git, not here.

## Durable session ownership

With `routing.session-affinity: true`, each explicit session keeps one account
across model changes, credential priority changes, config reloads, and restarts.
The routing strategy chooses accounts for new sessions and permitted migrations.

- Bindings are saved to `session-bindings.json` beside the active config file,
  including when using `--config`. No extra file-path setting is needed.
- Only confirmed quota exhaustion for the account or requested model permits
  migration. The replacement stays the owner after the old account recovers.
- Generic HTTP 429, billing-only rejection, authentication failure, policy refusal,
  and temporary network errors keep the owner. Existing retry and wait limits
  apply. The fork adds no retry loop or request timeout.
- Claude's explicitly rejected Fable window
  (`Anthropic-Ratelimit-Unified-7d_oi-Status: rejected`) permits model-quota
  migration. Late successes and generic rate limits cannot erase or extend an
  active confirmed model-quota deadline. Migration moves the whole session.
- Bindings use the provider, downstream caller scope, and explicit session ID.
  Recognized aliases share an owner. Conflicting owners return
  `session_affinity_conflict`.
- With subagent affinity enabled, a new child inherits a known parent's owner.
  Its binding can then migrate independently of the parent and siblings.
- Bindings do not expire. `session-affinity-ttl` only controls the memory cache.
  Disabling affinity retains the file; enabling it again restores saved owners.
- Selection, retries, and WebSocket alternate-account probes all enforce the
  saved owner. Antigravity credits fallback is disabled in durable mode because
  it bypasses normal account selection.

Clients must reuse a stable explicit ID, such as `X-Claude-Code-Session-Id`,
`X-Session-ID`, or a supported body identity. This also applies to token counting.
Missing identities return `session_id_required`. Message-content matching cannot
establish durable ownership.

Missing, disabled, or model-incompatible owners return an error. If quota permits
migration but no replacement is eligible, the old binding remains. Keep credential
IDs stable. Account affinity reduces unnecessary Claude thinking loss; it cannot
transfer thinking blocks between unrelated accounts.

## Soonest weekly reset first

Set `routing.strategy: soonest-reset-first` to use Claude OAuth quota nearest its
weekly reset first. Manual priority still applies. Existing durable owners stay
fixed. Only new bindings and migrations already allowed by confirmed quota
exhaustion use the ranking. Other providers and mixed-provider candidate sets use
round-robin.

Normal inference headers refresh each observed window without extra requests.
A single background worker reads each account at most once per 15 minutes and
spaces reads across accounts by 30 seconds. Recent shared-window headers suppress
reads. A valid full response with a null or missing five-hour window confirms
that absence for 24 hours. Missing headers alone cannot establish it. New explicit
five-hour headers replace this cached capability immediately.

Known future weekly resets remain useful for ranking for up to 24 hours. Only
usage observations less than five minutes old can exclude accounts from new
selection for exhaustion. Expired resets are ignored. Unknown reset data uses
round-robin after accounts with known resets. A probe failure retains previous
data and cannot authorize migration.

A quota-endpoint 429 pauses the entire background polling pool for at least
30 minutes, or longer when `Retry-After` requires it. This shared cooldown survives
restarts and credential changes. Other failures back off from 15 minutes to two
hours. A successful read cannot restore one-minute polling. Three quota auth
failures pause that grant; an unchanged terminal refresh rejection pauses it
immediately. Only an expired-token quota 401 invokes coordinated token refresh.
A quota rejection of a valid token does not invalidate inference. Warnings include
the safe failure category, HTTP status when available, failure counts, and retry
time. They never include raw response bodies or transport errors.

Snapshots and retry gates persist in local `kv.db` when KV storage is enabled.
They use hashed credential keys and expire after eight days. Successful token
refresh keeps observations. Manual credential replacement starts a new account
cache record but preserves the shared pool cooldown. Quota reads use the
credential's proxy and a fixed Anthropic endpoint. API keys and Home are excluded.
When all eligible accounts are observed as exhausted, new selection returns 503,
including accounts with paid extra usage. Existing owners still run as usual.
One stalled quota response can delay the single worker until shutdown; normal
response headers still update quota. Large account pools take longer to refresh.
Unused windows with null reset times have no reset available for ranking.

## Configuration

Merge these settings into the existing config. Do not duplicate YAML sections.

```yaml
routing:
  session-affinity: true
  session-affinity-subagents: true
  cooldown:
    disable-cooling: false
    save-cooldown-status: true

oauth:
  auth-dir: /root/.cli-proxy-api

upstream:
  claude:
    model-level-cooling: false
```

Affinity defaults to `false`. These are existing config options; the fork changes
what enabled affinity does. No new management control is required.

Keep cooldowns enabled, including provider and credential overrides. Claude's
`model-level-cooling: false` keeps shared quota rejections account-wide. Explicit
Fable rejection can still authorize model-quota migration.

`save-cooldown-status` preserves reset deadlines in `.cds` files beside credentials.
Ownership persistence is separate. Without saved cooldowns, a restart can try an
exhausted owner before a new response confirms exhaustion.

## Other fork patches

- **Database startup:** return a startup error if the enabled local key-value
  database cannot open. Hot reload logs errors and can recover with a valid path.
- **Usage identity:** prevent delayed stream usage from reading a Gin request
  context that has been reused for another request.
- **Stream logging:** join the logging worker before clearing its chunk channel.
- **Claude setup tokens:** read boolean credential metadata under its lock.
- **Git storage recovery:** recognize missing pack or index files by their paths,
  without treating unrelated missing files as repository corruption.
- **Home latency reporting:** use a separate cancelable connection for reports
  and remove added probe/report deadlines. Durable ownership still excludes Home.
- **Citation cleanup:** mutate Claude web-search citation JSON through one
  `ojson.Doc` and serialize once.

Additional regression tests cover durable ownership, quota classification,
shutdown and resume, saved-file compatibility, payload-rule ordering, concurrency,
and the fixes above. The JSON mutation change preserves citation behavior.

## Docker deployment

The fork publishes Linux x86_64 images to
`ghcr.io/raymond-w-ko/cliproxyapi`. Use this deployment's `compose.yaml`:

```yaml
services:
  cliproxyapi:
    image: ghcr.io/raymond-w-ko/cliproxyapi:latest
    platform: linux/amd64
    command: ["./CLIProxyAPI", "--config", "/etc/cliproxy/config.yaml"]
    restart: unless-stopped
    ports:
      - "127.0.0.1:8317:8317"
    volumes:
      - ./config:/etc/cliproxy
      - ./auths:/root/.cli-proxy-api
      - ./logs:/CLIProxyAPI/logs
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

Put `config.yaml` and any existing `session-bindings.json` in `./config/`.
Retain credentials, cooldown files, and `kv.db` in `./auths/`. Keep the config
directory outside the watched auth directory.

Mount the writable config **directory**. Each ownership change saves through a
temporary neighbor file and atomic rename. Mounting only `config.yaml` loses the
bindings on container replacement. Mounting the bindings file itself prevents
atomic replacement. An absent bindings file is initialized automatically; do not
create an empty one.

```bash
docker compose -f compose.yaml pull
docker compose -f compose.yaml up -d
```

Stop the proxy before moving or backing up state. Preserve both `./config/` and
`./auths/`, including `kv.db`. An explicit database path must also use persistent
storage.

## Storage limits and recovery

Use one process and one SDK Manager per bindings file. Home, scheduler plugins,
and routes with multiple actual providers are unsupported. The internal `mixed`
selection path works when it contains one actual provider.

New bindings files have mode `0600`. They contain hashed session keys and credential
IDs. Each ownership or alias change rewrites the full map before dispatch. This
is intended for modest single-process deployments.

Read, format, or write errors stop durable routing. Repair storage and restart.
Do not edit bindings while the proxy runs. Deleting the file resets ownership
and permits new account choices. See [selective binding recovery](docs/durable-session-affinity.md#recovery-and-scope)
for removing one binding and its aliases safely.

## Affinity logs and SDK lookup

Info logs include `durable binding created` and `durable binding migrated` only
after persistence succeeds. Migration records the previous and replacement
credential IDs, model, quota reason, and reset deadline. The `binding` field is a
12-character prefix of the stored hash. It does not expose the raw session ID.

Debug logs include `durable binding retained` and memory-cache events. A memory
cache miss does not mean the saved owner changed.

The Go API `Manager.LookupSessionAffinity(provider, model, sessionID)` inspects
only the memory cache and lacks caller scope. It cannot reliably inspect durable
ownership. Normal HTTP routing uses the durable store. See
[Durable session affinity](docs/durable-session-affinity.md) for implementation details.

## Image publication

The [Fork GHCR image workflow](.github/workflows/fork-ghcr.yml) runs on `main`
pushes and manual dispatches. It publishes `latest` and `sha-<full commit SHA>`
for `linux/amd64`, using GitHub's temporary token. Actions are pinned to commits.

Publication keeps `latest` and the two newest previous builds, including their
platform and attestation manifests. Older tags and digests become unavailable
for fresh pulls. Unexpected tags, unreadable manifests, or a mismatched `latest`
stop cleanup before deletion. Keep a separate image copy for older rollbacks.

Only this publisher is enabled in the fork. The six inherited workflows remain
disabled in GitHub settings; their source files stay unchanged for upstream
merges. A new fork must apply these settings separately. Review new upstream
workflows before enabling them.

The job runs only in `raymond-w-ko/CLIProxyAPI` on `main`. A new fork must adapt
that guard and the image owner in the workflow and cleanup script. Set the GHCR
package to Public for anonymous pulls; source repository visibility does not
set package visibility.
