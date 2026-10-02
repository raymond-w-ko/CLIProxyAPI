# Durable session affinity

For a single standalone CLIProxyAPI process, durable affinity keeps each session on
one credential across model changes, configuration reloads, and restarts. The
existing session-affinity toggle controls this behavior; no new management UI or
configuration field is needed.

```yaml
routing:
  strategy: round-robin
  session-affinity: true
  session-affinity-subagents: true
```

When enabled, bindings are saved to `session-bindings.json` in the same directory
as the active configuration file, including when a custom `--config` path is used.
Bindings have no TTL, including when `session-affinity-ttl` is set. The routing strategy chooses
credentials for new sessions and permitted migrations; existing owners take
precedence over credential priority and model changes.

Disabling `session-affinity` restores normal routing and failover without deleting
saved bindings. Re-enabling it restores those owners. Requests made while affinity
is disabled may use different accounts. Durable ownership is enforced only while
affinity is enabled. The default remains `false`.

## Session identity and scope

Clients must supply a stable explicit session ID, such as
`X-Claude-Code-Session-Id`, Claude Code's session in `metadata.user_id`, or
`X-Session-ID`. Use the same ID after client restarts and compaction. Requests
without an explicit identity return `session_id_required`; inferred message hashes
and prefix matching do not provide durable ownership. This also applies to token
counting requests. Existing explicit body and execution-session identities are
accepted by the normal session extractor.

Bindings use the provider, downstream caller scope, and session identity, but not
the model. Changing the downstream caller scope creates a separate binding. The
file contains hashed binding keys and stable credential IDs, not tokens or
conversation content. Credential IDs must remain stable: removing or renaming an
owner does not authorize migration.

Recognized aliases, such as a `prompt_cache_key` paired with a `conversation` ID,
share one persisted owner. A migration updates their shared binding. If a request
joins identifiers that already belong to different accounts, it fails with
`session_affinity_conflict` instead of choosing one silently.

When subagent affinity is enabled, a new child inherits a known parent's owner.
Children then have independent durable bindings. A child's later migration does
not move the parent or siblings.

## Failures and migration

Normal errors, network failures, generic HTTP 429 responses, and authentication
failures retain ownership. The existing retry budget, credential refresh,
cooldowns, and cooldown wait limits continue to apply, but
retry selection cannot switch to an unrelated healthy credential. No new network
timeouts or retry loops are introduced.

Migration is allowed only while the current owner has an active, confirmed quota
cooldown for the whole credential or the requested model. Claude's explicit
`Anthropic-Ratelimit-Unified-7d_oi-Status: rejected` signal authorizes migration
for the Fable/overage-included model window without cooling healthy sibling
models. Its model-specific reset deadline is honored when supplied. A generic
`rate_limit_error` body, `Retry-After` alone, or disabled overage alone is not proof.
Other providers retain their existing credential-wide classification unless they
explicitly report model-quota exhaustion. Disabling cooldowns prevents migration.
Late successful responses do not clear an active confirmed model cooldown. A
longer generic cooldown still delays retries, but does not extend the confirmed
model-exhaustion deadline that authorizes migration.

Keep Claude's `upstream.claude.model-level-cooling: false` so shared quota
rejections cool the entire account. Setting it to `true` scopes even shared
rejections to the requested model; an explicit quota rejection can still authorize
migration for that model. Migration always moves the whole session, not just one
model's requests.

If all accounts are exhausted, existing cooldowns exclude them from selection.
`routing.retry.request-retry` bounds additional rounds and
`routing.retry.max-retry-interval` caps each cooldown wait, not total request time.
The proxy returns 429 when the budget is spent or the next wait exceeds that cap.
Later client requests with cooldowns beyond that cap fail locally without
upstream traffic.
Clients should honor `Retry-After` when present and otherwise use exponential
backoff with jitter. No background retry loop is introduced.

Previously saved generic `quota` cooldowns do not prove model exhaustion and are
not upgraded automatically. After their cooldown expires, a fresh upstream response
can establish the confirmed `model_quota` state, which existing cooldown
persistence saves across restarts.

Ownership persistence and cooldown persistence are separate. Enable the existing
`routing.cooldown.save-cooldown-status` setting to retain known reset deadlines
across restarts. It defaults to `false`; without it, a restart can probe a still
exhausted owner again before recording a fresh quota response.

Antigravity's optional credits fallback bypasses normal account selection and
cooldowns. Durable mode disables that fallback so it cannot switch accounts or
retry outside the ownership policy. Normal quota-authorized selection remains
available.

After migration, the replacement remains the owner even when the old account
recovers. If no replacement is eligible, the old binding remains. Missing,
disabled, or model-incompatible owners return an error without switching. An
explicit credential pin that conflicts with a non-exhausted owner also fails.

Account affinity reduces unnecessary Claude thinking loss. It cannot make thinking
blocks portable between unrelated accounts. Keep the full conversation history;
Claude determines which thinking blocks it can use after a permitted migration.

## Storage and deployment

### Routing logs

At info level, `session-affinity: durable binding created` and
`session-affinity: durable binding migrated` are emitted only after the binding is
successfully persisted. Each includes a 12-character prefix of the stored binding
hash, the credential ID, provider, and requested model. Aliases share the same
binding hash. Migration also includes `previous_auth`, `reason` (`credential_quota`
or `model_quota`), and the authorizing `quota_reset` deadline in UTC. Request IDs
connect these events to upstream failures in the same request.

With debug logging enabled, `durable binding retained` identifies successful
selection of an existing owner. Memory-cache events are explicitly labeled and
logged only at debug level in durable mode; a memory-cache miss does not mean the
persisted owner changed. Failed selection or persistence never emits a successful
migration event. Durable ownership logs do not include raw session IDs or tokens.

### Persistent files

Keep the configuration directory writable and persistent, outside the watched
`auth-dir`. For example, `--config /etc/cliproxy/config.yaml` stores bindings at
`/etc/cliproxy/session-bindings.json`. New files have mode `0600`; the store uses a temporary file, file
sync, and rename, plus directory sync on platforms that support it. Writes occur
only when a binding is created or changes, not on every request. The complete map
is rewritten on each change, so this is intended for modest single-process use.

### Docker

The repository's default Compose file mounts only `config.yaml`. That does not
persist a neighboring bindings file when the container is recreated. To enable
durable storage, put your configuration in a host directory such as `./config`,
replace the existing config-file mount with a directory mount, and select that
configuration path:

```yaml
services:
  cli-proxy-api:
    command: ["./CLIProxyAPI", "--config", "/etc/cliproxy/config.yaml"]
    volumes:
      - ./config:/etc/cliproxy
      # Retain your existing auth, log, and plugin mounts.
```

This directory must contain `config.yaml` and allow the proxy to create and rename
files. Mount the directory, not an individual `session-bindings.json` file, because
atomic replacement requires creating a temporary neighbor and renaming it.

### Recovery and scope

The store loads before the first durable selection. Selection and persistence are
serialized; upstream execution is not. A new owner is persisted before dispatch.
Read, format, or write failures stop durable routing instead of silently falling
back to another account. Repair storage and restart after such an error.

One process and one SDK Manager may own a file. Sharing the file between processes
or Managers is unsupported. Home, scheduler plugins, and a model route containing
multiple providers return `session_affinity_unsupported`. The ordinary internal
`mixed` selection path with one actual provider is supported.

Bindings survive selector reloads within a Manager. Do not edit the file while
the process runs. To deliberately reset a binding, stop the process, back up the
file, remove the desired entry from `bindings` and any `aliases` that refer to it,
and restart. Removing the file resets all ownership
and permits new account choices. Disabling the existing `session-affinity` toggle
leaves the file intact. Management-page saves of the YAML do not modify this file.

## Release-note context

The existing `routing.session-affinity` toggle now enables persistent,
provider-wide ownership, stored beside the configuration file with no new setting.
Deployments with affinity already enabled gain durable ownership and quota-only
migration, and now require explicit session IDs and writable persistent config
storage. Deployments with affinity disabled retain normal routing. Existing retry
timing is unchanged.
