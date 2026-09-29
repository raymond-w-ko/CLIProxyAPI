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

Normal errors, network failures, generic HTTP 429 responses, authentication
failures, and model-specific limits retain ownership. The existing retry budget,
credential refresh, cooldowns, and cooldown wait limits continue to apply, but
retry selection cannot switch to an unrelated healthy credential. No new network
timeouts or retry loops are introduced.

Migration is allowed only while the current owner has an active, confirmed
credential-wide quota cooldown. Claude already classifies shared subscription
window rejections from upstream rate-limit headers separately from model-specific
or overage-only rejections. A generic `rate_limit_error` body is insufficient.
Other providers migrate only if their existing error handling establishes the same
credential-wide quota state. Disabling cooldowns prevents this migration signal.

Claude's `oauth.providers.claude.model-level-cooling: true` also suppresses this
signal by deliberately treating shared quota rejection as model-scoped. Leave it
`false` to allow migration on confirmed shared quota exhaustion. Durable affinity
does not override your cooldown classification settings.

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
