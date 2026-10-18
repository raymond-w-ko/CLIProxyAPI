# Fork notes: durable session affinity

This fork keeps each session bound to one account across model changes, config
reloads, and restarts. Only confirmed quota exhaustion for the account or requested
model permits a switch. Temporary errors, repeated failures, and generic HTTP 429
responses keep the binding. The existing retry budgets, refresh, cooldowns, and
wait limits still apply; this patch adds no retry loops or network timeouts.

After a permitted switch, the replacement remains the owner even when the old
account recovers. Account affinity reduces unnecessary Claude thinking loss, but
cannot transfer thinking blocks between unrelated accounts.

## Recommended configuration

Merge these settings into your existing `config.yaml`. Preserve the other settings
and do not create duplicate `routing`, `oauth`, or `upstream` sections.

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

These are existing config options, not additional code changes or new management
page controls:

- `session-affinity: true` enables this fork's durable ownership. Its default
  remains `false`.
- `session-affinity-subagents: true` lets a new child inherit a known parent's
  account. Children then have independent bindings and may migrate independently.
- `save-cooldown-status: true` persists known quota reset deadlines as `.cds` files
  beside the auth files. Binding persistence alone does not preserve those
  deadlines; without cooldown persistence, a restart may probe an exhausted account.
- Cooldowns must remain enabled, including any provider or credential overrides.
  Leave Claude's `model-level-cooling` at `false` so shared quota exhaustion cools
  the entire account. Explicit Fable quota exhaustion remains model-scoped and can
  authorize session migration without cooling healthy sibling models.

No `session-affinity-file` setting is needed. The file is always
`session-bindings.json` beside the active config file, including with `--config`.
Management page saves of the YAML do not modify this file.

## Upstream shared provider settings update

Upstream commits `52d5507d` and `3be5fa44` move shared Claude, Codex, and xAI
settings into `upstream.<provider>`. These settings now apply to both OAuth and
API-key credentials. OAuth-only settings and `oauth.auth-dir` remain under
`oauth`.

For this deployment, the recommended Claude setting is now
`upstream.claude.model-level-cooling: false`. Historical
`oauth.providers.claude.model-level-cooling` remains accepted, so an immediate
manual edit is not required. The canonical path wins when both are present,
including explicit `false`. Saving an existing v8 configuration normalizes it
to the current layout; historical v8 management paths remain supported.
Durable bindings and quota-only migration are unchanged.

## Upstream apply_patch compatibility update

Upstream commit `3ebee065` adds a compatibility bridge for Codex's custom,
freeform `apply_patch` tool across Claude, Gemini, and other supported executors.
It wraps patch text in a JSON function argument named `input` for upstream models
and restores Responses custom-tool events for the client. Native Codex keeps its
native protocol. The proxy does not execute patches or edit files; the client
still performs the tool call. The bridge validates argument encoding, call
identity, and completion, not whether the patch itself is correct.

Commit `d306f2c5` restores native template advertisement by default after
`63c04b4b` had made all advertisement opt-in. To advertise the bridge for
supported non-native models to clients that use `/v1/models?client_version=`,
keep this setting enabled:

```yaml
client:
  codex:
    enable-apply-patch: true
```

The default is `false`, which retains `freeform` only where the model template
already declares it (excluding image/video and other non-text entries).
This is an advertisement switch, not a tool-execution or translation guard:
explicitly supplied custom tool declarations can still use the bridge. When
enabled, advertisement requires every executor for the public model to support it.

`optimize-multi-agent-v2` also moves to `client.codex.optimize-multi-agent-v2`.
Legacy YAML paths migrate on load; the new path wins when both are present,
including explicit `false`. The setting now applies to API-key and OAuth routes.
These upstream settings do not change durable ownership or quota-only migration.

## Upstream Claude continuity and refresh update

Claude's injected date originally stayed fixed while its in-memory continuity
entry survived, avoiding prompt-cache invalidation at midnight. The later update
through `911f49cb` retains the date independently for seven days by default, as
described below. Date pins remain separate from durable account ownership and
are not persisted across restarts.

An upstream 401 followed by an invalid refresh grant now keeps the credential
unavailable instead of repeatedly retrying it. A new login or a successful
explicit refresh is needed for recovery. Durable affinity still does not migrate
on authentication failure alone. Claude refresh also stops replaying ambiguous
transport or decoding failures because its single-use refresh token may already
have been consumed.

## Upstream Antigravity catalog update

The bundled Antigravity catalog includes `claude-opus-5-5-high` and
`claude-sonnet-5-5-high`, advertising a 1,000,000-token context and 128,000 maximum
completion tokens. Upstream subsequently restored `claude-opus-4-6-thinking` and
`claude-sonnet-4-6`; the earlier advice to replace all 4.6 aliases is no longer
required by the bundled catalog. Actual availability depends on the account's
model entitlements. These changes do not rename direct Claude provider models
or change durable account ownership.

## Native Claude Code and Codex update

The upstream changes through `a4acc9f7` affect native clients when they use this
proxy, even without cross-provider translation:

- Claude thread continuations reserve one cache breakpoint and retain OAuth tool
  aliases. Missing thread state returns `thread_not_found` so a supporting client
  can replay the full conversation. The alias cache is in memory and can be lost
  on restart or eviction; durable account bindings remain separate and persistent.
- Codex WebSocket activation now receives disconnect errors that previously could
  be lost between connection setup and request activation.
- Credential mutations and persistence are coordinated to avoid losing concurrent
  updates. Successfully refreshed tokens are saved even if the request was canceled.
- User payload rules now run after built-in request transformations. If you use
  payload overrides or filters, review them: later built-in cleanup no longer
  restores filtered fields or replaces explicitly configured values.

No new setting is required for the recommended deployment. Optional
`models.catalog`, `models.codex-catalog`, and `models.devin-catalog` accept custom
catalog sources; leave them unset to retain the default sources. Clients that
connect directly to their providers, bypassing this proxy, are unaffected.

The follow-up through `a2976eb8` fixes sharing of Claude's tool alias cache across
per-request executor copies; the cache remains in memory. Claude requests can
also supply `"prompt_cache_options":{"mode":"explicit"}` to keep caller-owned
cache markers and TTLs instead of automatic injection and TTL normalization.
The proxy still enforces its cache-breakpoint limit and strips this proxy-only
option before sending upstream. This is a request-body option, not a new YAML
setting, and the recommended deployment does not need to enable it. Codex usage
records now report reasoning effort after payload overrides are applied.

The update through `f3703e82` strengthens credential concurrency handling. Results
from a superseded credential version or registration no longer change current
availability or cooldowns, and an in-flight refresh cannot overwrite newer
credentials. Refresh preserves already-recorded active quota cooldowns. Fork
regressions verify that a stale quota response does not move durable ownership,
while a confirmed model quota recorded before refresh still permits migration.

Claude also recognizes missing thread state in plain-text and structured errors,
and JSON errors are compacted for SSE delivery. Confirmed native Claude Code
retains its passthrough system prompt. The caller system-prompt relocation changes
apply to cloaked requests, including other clients routed through Claude OAuth;
native Codex routed to Codex does not use that path. Grok speech endpoints and
Gemini translation fixes do not affect native Claude/Codex routes.

No configuration change is required. The new optional `server.github-token` is
for the proxy's GitHub downloads and API requests, not model authentication or
GHCR publication. Leave it unset unless those downloads need authentication;
existing environment-token fallback remains supported.

The update through `0f96f568` improves attachment handling during protocol
translation. Supported images and files retain more of their source content.
When unsupported attachments would leave a user turn with nothing sendable, the
proxy returns HTTP 400 before contacting the provider. If the same turn retains
sendable text or other content, unsupported attachments can still be omitted.
This is not a guarantee that every attachment reaches every provider. These
request errors do not authorize account migration; fork regression coverage
checks unchanged persisted ownership for execution, streaming, and token counting.
Native Claude-to-Claude and Codex-to-Codex conversion behavior is unchanged, but
cross-provider routes used by either client can encounter the new rejections.

The service also starts a background Grok CLI version lookup against npm at
startup and every three hours, even without an xAI credential. It uses the global
outbound proxy setting and retains its cached/fallback version on failure. This
does not change Claude or Codex request versions. No configuration change is
required for this deployment. Embedded SDK users registering request translators
must adapt to the new `([]byte, error)` return signature; HTTP clients need no
such change.

The update through `54946fa3` reconciles account changes during batch model
registration. Concurrent model exclusions now reach the registry and scheduler
instead of being replaced by a stale snapshot. This applies to native Claude and
Codex accounts too. Durable ownership still takes precedence: if the owner loses
access to the requested model, the request fails rather than moving to another
account without confirmed quota exhaustion. A fork regression covers this case
and verifies that the persisted binding remains unchanged.

Other changes add opt-in native Vertex Interactions routing, preserve foreign
reasoning content on Meta routes, and normalize YAML collection formatting in
config saves and the v8 config API. Native Claude/Codex request handling needs no
configuration change; leave Vertex `interactions` unset unless using that API.

The update through `67465884` ignores overage billing boundaries when calculating
Claude rate-limit recovery. A rejected overage claim with absent, unexhausted
shared windows no longer counts as account-wide quota exhaustion. Explicit
`7d_oi` model rejection still permits this fork's model-quota migration and uses
its applicable reset deadline. Reset candidates beyond seven days plus one hour
are discarded. Existing persisted cooldowns are not rewritten by this parser
change. Regression tests cover billing-only rejection and model rejection with a
billing reset across execution, streaming, token counting, and compaction.

Codex WebSocket clients can now send `response.interrupt` during a response. The
proxy forwards it to the active Codex socket without selecting another account;
HTTP-backed turns use local cancellation. An interrupted response can be followed
by another turn on the same downstream connection.

Claude routes now support Responses compaction, including `/responses/compact`,
using a generated summary in a proxy-specific capsule. This mainly affects Codex
or other Responses clients routed to Claude, not native Claude Messages requests.
Foreign compaction capsules are dropped with a warning rather than decoded, so
do not rely on native Codex compaction capsules carrying context into Claude.
For Responses requests without an output limit, registered non-Fable Claude
models now use the registry completion limit; Fable retains its conservative cap.
No configuration change is required for the recommended deployment.

The update through `d318bcc3` makes late Codex WebSocket interrupts harmless when
their response has already finished on the retained upstream connection. They no
longer leave an error for the next turn or interrupt a newer response. Active
interrupts still use the current connection without selecting another account.
Interrupt timeline diagnostics record a hashed response ID and outcome instead
of arbitrary control-frame fields.

Claude system-prompt insertion now preserves directive-only effort changes
between user turns. Claude also repairs non-portable tool-call IDs consistently
across calls and results before user payload rules run; valid native IDs remain
unchanged. This mainly helps conversations replayed from other providers. When
Claude Code uses an OpenAI-compatible backend, final streamed usage now waits
for the trailing usage chunk or `[DONE]`, retaining authoritative cache counts.

Direct OpenAI image routes now preserve the resolved upstream model name,
including configured aliases. Home dispatch skips local availability filtering
because Home owns that decision; ordinary proxy routing retains its existing
filtering and this fork's durable quota-only migration. Home remains unsupported
with durable affinity. No configuration change is required for this deployment.

The update through `3de4e248` preserves upstream cooldown errors when stream
failure and request cancellation happen together. Codex WebSocket errors reach
the conductor before disconnect notification closes the downstream connection.
The canceled request still stops; a later request can migrate only if the saved
error confirms account-wide or requested-model quota exhaustion. A generic 429
with `Retry-After`, a temporary error, or client cancellation alone still cannot
authorize migration. Fork regressions cover direct stream errors and bootstrap
error chunks, including persisted ownership after restart.

Claude cache handling now ignores null and invalid cache-control entries when
counting breakpoints, inserting automatic markers, and normalizing TTL order.
This does not guarantee removal of every invalid caller-supplied value. Codex's
Chat Completions translation shortens tool-call IDs longer than 64 bytes while
keeping calls and results matched; native Codex Responses requests do not use
that translation. Usage records now include the downstream authentication
provider and whether it is a built-in API key. These fields describe client
authentication, not the upstream Claude/Codex account or durable owner. No new
configuration is required for the recommended deployment.

The update through `f1d4a94e` makes global model metadata lookup use the active
Devin and Meta catalog getters. Devin lookup now follows refreshed catalog
metadata instead of reviving models from an inactive fallback catalog. Native
Claude/Codex catalog entries and durable affinity are unchanged. The remaining
upstream changes update sponsorship documentation; no configuration change is
required.

The update through `091ff27f` adds optional Codex client tool search on Claude
Responses routes. `upstream.claude.tool-search` defaults to `false`; leave it
unset for native Claude Code-to-Claude and Codex-to-Codex use. Enable it only
when using Codex with Claude and wanting client-executed deferred tool discovery.
Official Anthropic credentials qualify automatically for supported models when
enabled. Custom Claude API-key endpoints require `native-tool-search: true` on
the key to opt in; legacy Claude models remain excluded.
Unsupported endpoints use the earlier translation, which drops tool-search
history rather than running search elsewhere. The Codex catalog advertises
support only when every serving credential qualifies. The setting is pinned for
each request so config reloads cannot change translation midway through it.

Antigravity now persists model and web-search capability hints in auth metadata
and reuses them after restart while refreshing them in the background. These
hints are separate from durable session bindings. Shared credential persistence
now lets foreground credential saves preempt background hint saves; a fork
regression verifies that a quota result arriving during metadata persistence
still permits migration only for confirmed model exhaustion. Gemini/Antigravity
also fix trailing reasoning-only turns and tool results containing media.
For the recommended native Claude/Codex deployment, no configuration change or
immediate upgrade is needed for these features.

## Upstream Codex lifecycle and affinity update

The merge through `bf934162` preserves live Codex WebSocket sessions when config
reloads replace an executor sharing the same session store. Removing a credential
closes only that credential's sessions. Existing connections retain their original
configuration; new connections use the updated configuration. Codex credential
refresh and persistence now use file locks, atomic writes, and token-aware merges
to avoid overwriting newer credentials during concurrent saves.

Upstream adds `routing.session-affinity-rebind-on` (`any` by default, or `quota`).
Neither value relaxes this fork's durable ownership policy. Upstream's memory-only
`quota` mode permits temporary execution on another account and rebinding for
several authentication and availability errors. The fork still permits migration
only for confirmed account or requested-model quota exhaustion. Regression tests
cover both settings, including execution, token counting, and streaming. No new
affinity setting is required for this deployment.

The model registry now recognizes `credential_quota` alongside model quota when
calculating availability. Exhausted credentials no longer make an otherwise known
model disappear from the catalog solely because all its credentials are cooling.

Other additions remain opt-in:

- Sign in with ChatGPT (SIWC) uses a separate nested token partition and requires
  explicit `use_siwc` activation. Existing Codex credentials retain their normal
  endpoint unless enabled. The shared credential persistence improvements still
  apply to ordinary Codex credentials.
- `server.max-request-body-bytes` defaults to `0` (disabled). A positive value
  rejects oversized request bodies with HTTP 413.
- `requests.streaming.send-keepalive-before-ft` defaults to `false` and requires
  a positive `keepalive-seconds`. Enabling it can send SSE heartbeats before the
  upstream response starts. The first heartbeat commits HTTP 200, so later errors
  must travel inside the stream rather than changing the HTTP status.

Plugin quota/auth changes and Gemini schema cleanup do not require changes for
native Claude Code and Codex use without plugins. Keep the new optional features
at their defaults unless needed. This update is useful for Codex reload and token
reliability, but does not require an urgent deployment or a config migration.

## Upstream reload, streaming, and model update

The merge through `0d18d3b3` includes 28 upstream commits. The most relevant
changes for native Claude Code and Codex use are:

- Config snapshots and synchronized access reduce races during startup, model
  registration, request handling, and hot reload. Durable binding paths and
  quota-only ownership remain unchanged.
- HTTP/2 closed-connection errors now use the existing bounded transport retry
  path without cooling an otherwise healthy credential. A fork regression checks
  that these retries stay on the durable owner.
- The Codex HTTP executor assembles SSE events split over multiple `data:` lines,
  including completion events, while preserving its bootstrap buffering limits.
  Native upstream WebSocket frames do not use this SSE parser.
- Codex tool-schema normalization removes integer bounds outside signed 64-bit
  range that can cause upstream HTTP 500 responses. User payload rules remain
  the final semantic step.
- Claude OAuth tool-name recovery recognizes caller-owned MCP tools that the
  model wraps with another MCP prefix. The model catalog adds Haiku 5.5.

Cross-provider changes include Gemini client-side tool search, better union
schema handling, Kimi namespaced tools, and unique fallback Responses item IDs.
Claude Chat Completions now uses model-specific default output limits, and
configured default token limits can override translator-generated defaults.
Native Claude Messages requests retain their existing behavior. Catalog updates
also add Gemini 4 Argon and Muse Wonder entries and remove older Gemini entries.
The new request-scoped HTTP transport decorator is an SDK hook; the standalone
proxy does not enable it automatically. No config migration is required.

Validation also exposed three small existing concurrency defects fixed in this
merge: a config write during reload could be incorrectly marked as already
loaded; streaming log finalization could clear the worker's queue before it read
pending chunks; and a late WebSocket interrupt could incorrectly enter local HTTP
cancellation handling. Regression coverage preserves pending config writes,
drains logs before closing, and keeps HTTP cancellation separate from duplex
WebSocket handling. Tests using removed Gemini models now use current entries.

The fork's GHCR workflow still retains the latest build and two previous builds,
including their referenced manifests. Inherited upstream workflows stay disabled.

## Upstream registration and TLS establishment update

The merge through `5fc580b1` includes 12 upstream commits. Native Claude Code and
Codex benefit from safer credential-file removal and model registration: delayed
work can no longer overwrite a newer registration, and a delayed file-delete
event checks the current file before removing credentials that were recreated.
Unreadable or invalid replacement files retain the current credentials until
their state can be determined. Durable ownership and quota-only migration are
unchanged; removed owners still fail closed.

Upstream also adds an optional TLS handshake limit:

```yaml
requests:
  tls-handshake-timeout-seconds: 0
```

The default `0` leaves this limit disabled. A positive value bounds only TLS
establishment on the shared uTLS Anthropic and ChatGPT/Codex HTTP paths. It does
not bound TCP dialing, proxy CONNECT, response generation, or streaming, and the
standard fallback transport does not use it. Enable it only if stalled TLS
handshakes are a problem. A configured handshake timeout uses the existing retry
budget without cooling the credential. A fork regression verifies that durable
retries keep the same account; this timeout never authorizes migration.

Other changes update Gemini 3.8 and Muse Wonder catalogs, Antigravity's client
version, tool schemas, cached-token accounting, and Gemini-to-Claude error and
tool-call translation. Plugin configuration defaults also improve. These changes
have little direct relevance to native Claude Code and Codex without plugins.
No configuration migration or urgent deployment is required.

Race validation exposed an existing Claude setup-token metadata read racing
with account-profile writes. The fork now uses the same metadata lock for those
boolean reads and covers them in the concurrent-reader regression. Validation
also retains the known local multicast-discovery integration-test limitation;
it is separate from HTTP proxy routing.

The GHCR publisher and its three-build retention policy remain unchanged. Only
the fork publishing workflow is enabled.

## Upstream cooldown ordering and config-save update

The merge through `5be2cdab` includes 13 upstream commits (including two merge
commits). The changes most relevant to native Claude Code and Codex are:

- In-flight execution results now carry a dispatch sequence. A successful
  request dispatched before a newer cooldown was recorded cannot clear that
  cooldown when it finishes. This covers normal requests, streaming, and token
  counting. The fork still retains confirmed account/model quota deadlines even
  for later successes; the expanded regression covers both dispatch orders and
  results without a sequence. Generic 429s still do not authorize migration.
- Config reloads are serialized and track the exact bytes parsed. Management
  saves reject stale reload snapshots and hold the config lock through mutation
  and persistence. This extends the fork's earlier pending-write fix; its
  regression remains in place. Binding-file ownership and persistence are
  unchanged.
- `upstream.claude.header-defaults` gains `anthropic-beta-strip`,
  `anthropic-beta-append`, `inject-diagnostics`, and `client-request-id` controls.
  Defaults preserve the current request shape: no beta overrides, diagnostics
  injection enabled, and request-ID mode `on`. Leave these defaults for native
  Claude Code unless diagnosing a specific compatibility problem. Per-credential
  headers retain their documented precedence; these controls do not change
  session identity or durable account ownership.

Other changes fix Vertex API-key base URLs and trailing thought-only turns,
Gemini/Antigravity schema handling, xAI web-search options, and incomplete tool
results when translating OpenAI Responses requests to Claude. The catalog adds
`gemini-4-argon`. These have little direct relevance when each native client uses
its matching provider.

Remote-store bootstrap now hashes plaintext management keys before seeding Git,
Postgres, or object storage, and refuses ambiguous files that still contain the
plaintext elsewhere. This does not require changes to the local-file Docker
deployment. No configuration migration is required. The GHCR publisher remains
isolated, with the latest build and two previous builds retained.

## Upstream config concurrency and plugin reload update

The merge through `e7003d2a` includes four upstream commits. Config reads under
`/v8/management/config` now return an ETag. Clients can send that value in
`If-Match` on writes to reject stale edits with HTTP 412 instead of overwriting
another save. This protection requires client participation; writes without
`If-Match` remain unconditional. Query-log masking also covers `value`, which
can contain a client API key. Neither change modifies durable binding storage.

Claude-to-Responses streaming now validates and selects request metadata once
per stream instead of revalidating the full request on every chunk. This reduces
repeated work for long conversations routed from Responses clients to Claude;
native Claude Messages and native Codex routes do not use this translator.

For the opt-in Sign in with ChatGPT (SIWC) route, unsupported
`configuration_update` items are removed and their last reasoning effort is
promoted to the request level. Explicit model thinking suffixes retain precedence,
and user payload rules still run last. SIWC steering frames remove these items
without changing response-level reasoning. Ordinary Codex OAuth continues to
preserve configuration updates. No configuration migration is required.

Plugin libraries now load from content-addressed shadow copies so replacing a
plugin artifact can load its new code safely. Logical unload detaches callbacks
but keeps loaded Go runtimes mapped until process exit; repeated distinct plugin
builds can therefore retain memory until restart. This does not affect the
recommended deployment without plugins. Thanks to the upstream contributors;
their original commits and authorship are preserved by the merge.

Durable ownership, quota-only migration, the isolated GHCR publisher, and its
three-build retention policy remain unchanged.

Validation includes the server build, full test suite, affected-package race
checks, durable-affinity regressions, and all nine image-retention tests. The
full suite's only failure remains the known local multicast-discovery test.

## Upstream preserved-thinking and layered cooldown update

The merge through `911f49cb` includes five upstream commits. Claude requests that
replay signed thinking on supported preserved-thinking models now default
`thinking.block_binding.prefix_mismatch_behavior` to `drop_block`, provided the
thinking mode supports binding controls. The matching beta header is included.
This lets Anthropic discard mismatched thinking instead of rejecting the request
with HTTP 400; it does not preserve those discarded blocks or make them portable
between accounts. Caller-supplied behavior is retained, and final user payload
rules can override the value to `error` or filter the binding field. Durable
affinity remains independent: only confirmed quota exhaustion permits migration.

The new `upstream.claude.session-date-pin-days` setting defaults to `7`. It keeps
the cloaked currentDate reminder stable across continuity-cache expiry and
credential switches, with a sliding retention window. An explicit `0` restores
the previous continuity-scoped behavior. These pins are bounded, in-memory state;
expiry, eviction, or process restart can lose them. A valid caller reminder can
seed a new pin. The setting does not persist dates in `session-bindings.json`,
and no configuration edit is required for the default behavior.

Devin's explicit daily/weekly quota exhaustion now cools the whole credential and
uses observed reset deadlines when available. Exhausted Devin credentials can
revalidate quota hourly and recover early on a fresh positive observation.
Shared cooldown handling preserves each model's independent failure underneath
an account-wide cooldown, including across cooldown-file reloads. A fork
regression verifies that restoring these layers retains confirmed model-quota
deadlines, while generic rate limits never gain permission to move ownership.

Other changes remove schema-injected placeholder arguments when Gemini or
Antigravity returns tool calls to Claude clients, preserve distinct tool names,
and avoid reporting a canceled Meta stream as failed after its terminal event.
These changes have little effect on native Claude/Codex routes. Upstream commit
history and contributor authorship are preserved. The isolated GHCR publisher
and its latest-plus-two-previous-build retention policy remain unchanged.

The server build, affected-package race checks, durable-affinity regressions,
and all nine retention tests pass. The new layered-cooldown regression also
passes ten race-enabled repetitions. The full suite's only failure remains the
known local multicast-discovery integration test.

## Upstream stream completion and credential snapshot update

The merge through `2422c6b0` includes 25 upstream commits, including three merge
commits. The most relevant changes for native Claude Code and Codex use are:

- Claude streams that end without a valid completion now report an upstream
  error instead of silently succeeding. The Claude HTTP handler also detects
  missing completion across split SSE chunks. Several translated provider paths
  gain corresponding checks. This exposes interrupted responses to clients; it
  does not resume a response or authorize account migration. A fork regression
  checks both native Claude and Responses-to-Claude streams and reloads the
  durable binding to verify that truncation retains ownership.
- Explicit Claude per-minute request/input/output-token errors with a usable
  `Retry-After` and no quota-status headers no longer inherit an unrelated,
  hours-long unified reset. Other errors keep conservative reset handling.
  Expanded fork coverage verifies short cooldowns without account migration
  across execution, streaming, token counting, and compaction.
- File-derived credential updates carry a content digest. A delayed watcher
  snapshot cannot overwrite newer persisted credentials. Model registration
  also reconciles current state when concurrent changes invalidate its result.
  Stable credential IDs and durable ownership are unchanged.
- Claude preserved-thinking binding now handles replayed signed blocks when the
  caller omits `thinking`: it supplies adaptive thinking and the existing
  `drop_block` default, except for forced tool choices. Caller settings and final
  payload overrides still take precedence. Mismatched thinking can still be
  discarded; this is compatibility handling, not a guarantee of preservation.
- Thinking normalization uses Sonnet 5.5's `between_tools` mode for an off
  request. Models that cannot disable thinking fall back to their lowest
  supported effort with thinking display omitted. Dated/context-suffixed Claude
  variants inherit family capabilities through the canonical thinking pipeline.

Credential-specific, nonempty `excluded_models` lists now replace global OAuth
exclusions for that credential instead of merging with them. Missing, empty, or
null lists inherit current global rules, including after reload. Invalid file
entries retain usable rules rather than dropping the credential; write APIs
reject invalid values. Review custom exclusion lists if they previously relied
on the union with global rules. Excluding the durable owner's requested model
still fails closed rather than switching accounts without quota exhaustion.

Other changes improve plugin model ownership and exclusion handling, preserve
large plugin usage events, remove Meta subscription telemetry from client
responses, and speed up Antigravity schema cleanup. LAN discovery gains host
conflict resolution and interface binding fallbacks. No configuration migration
is required for the recommended deployment. Upstream authorship is preserved;
the isolated GHCR workflow and three-build retention remain unchanged.

Validation: the server build and nine retention tests pass. The full suite and
broad race checks fail only the known local multicast-discovery integration
test; the new discovery unit tests pass. Expanded affinity regressions pass five
race-enabled repetitions. They confirm that per-minute throttling and truncated
streams do not move durable ownership.

## Recommended Docker Compose deployment

Use an image built from this fork's patched source. Upstream
`eceasy/cli-proxy-api:latest` does not automatically include these changes.

GitHub Actions builds this fork for Linux x86_64 (`linux/amd64`) and publishes it
to `ghcr.io/raymond-w-ko/cliproxyapi`. The VM only pulls and runs the image; it does
not need enough RAM to compile Go. See the publishing notes below for workflow
controls and package visibility.

Save this as your deployment's `compose.yaml`:

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

Before starting, arrange the deployment directory as follows:

```text
compose.yaml
config/
  config.yaml
  session-bindings.json   # Created automatically; preserve it if already present.
auths/                   # Existing credentials and persisted cooldown files.
logs/
```

When moving an existing deployment, stop the old proxy first. Put its config and
any existing bindings file in `./config/`, and retain its `./auths/` directory.
Do not create an empty bindings file; an absent file is initialized automatically.
Keep the config directory outside the watched auth directory.

The config directory must be writable by the container. Mount the **directory**,
not an individual `session-bindings.json` file: persistence writes a temporary
neighbor and renames it over the destination. An individual file bind mount
prevents that atomic replacement. Mounting only `config.yaml` also fails to
persist its neighboring bindings file when the container is recreated.

Start with an explicit Compose filename to avoid selecting the repository's
separate `docker-compose.yml`:

```bash
docker compose -f compose.yaml pull
docker compose -f compose.yaml up -d
```

Bindings then persist at `./config/session-bindings.json` on the host. Preserve
both `./config/` and `./auths/` across container replacements and backups.

## Image publishing and workflow isolation

The [Fork GHCR image workflow](.github/workflows/fork-ghcr.yml) runs on pushes to
`main` and manual dispatches. Its job only runs in `raymond-w-ko/CLIProxyAPI` on
`main`. It uses a standard Ubuntu x86_64 GitHub-hosted runner, the existing
Dockerfile, and GitHub's temporary `GITHUB_TOKEN` with `contents: read` and
`packages: write`. No Docker Hub credentials or personal access token are needed
for publishing. Actions are pinned to commit SHAs.

Each successful build publishes `latest` and `sha-<full Git commit SHA>`. Pin the
commit tag or the digest from the run summary when you want controlled updates.
Builds are serialized, and Docker layers are cached in GitHub Actions.

After a successful publication, the same workflow keeps the build tagged `latest`
and the two newest previous builds by creation time. Cleanup enumerates all GHCR
version pages and preserves every manifest referenced by these builds, including
platform images and attestations. GHCR may therefore show more than three package
versions even though only three usable builds remain. Unreferenced versions are
deleted. An unreadable retained manifest, unexpected tag, or mismatched `latest`
digest stops cleanup before deletion; API failures fail the workflow visibly.

Older commit tags and digests are no longer available for fresh pulls after
cleanup. Already-running containers and locally cached images are unaffected.
There is no age grace period or deployment-pin exemption. Keep a separate image
copy if you need a rollback older than these three builds. The workflow uses its
temporary token and needs package admin access for deletion (normally granted to
the repository that publishes the package). Keep this workflow as the package's
only publisher so its existing concurrency group also serializes cleanup.

The six inherited workflows are disabled individually in this fork's GitHub
settings: `agents-md-guard`, `auto-retarget-main-pr-to-dev`, `docker-image`,
`translator-path-guard`, `pr-test-build`, and `release`. Their source files remain
unchanged for upstream synchronization. Do not enable all workflows when updating
the fork; only `Fork GHCR image` is needed. Workflow disablement is a repository
setting, so a new fork must apply it separately. Newly added upstream workflows
also need review before pushing them into this fork.

GitHub creates new GHCR packages as private by default, even for public source
repositories. After the first publication, set this package's visibility to
**Public** in its [package settings](https://github.com/users/raymond-w-ko/packages/container/cliproxyapi/settings).
Public visibility permits anonymous pulls on the VM. A private package requires
registry authentication. Verify anonymous access before relying on unattended
updates; repository visibility alone does not establish package visibility.

To trigger a rebuild manually:

```bash
gh workflow run fork-ghcr.yml --repo raymond-w-ko/CLIProxyAPI --ref main
```

For an optional local build on a stronger x86_64 machine:

```bash
docker build --platform linux/amd64 -t cliproxyapi-affinity:local .
```

Use `image: cliproxyapi-affinity:local` instead of the GHCR image for that deployment.

## Operational caveats

- **Stable session IDs are required.** Clients must send an explicit identity,
  such as `X-Claude-Code-Session-Id`, `X-Session-ID`, or a supported body identity.
  Reuse it after client restarts and compaction. Requests without an explicit ID,
  including token counting requests, fail with `session_id_required`.
- **Ownership spans models, but is scoped by provider and downstream caller.**
  Changing caller scope creates a separate binding. Recognized aliases share an
  owner; joining aliases with conflicting owners fails with
  `session_affinity_conflict`.
- **A 429 alone is not proof of exhaustion.** Claude needs the existing upstream
  header classification for shared quotas or an explicit rejected model window
  (`Anthropic-Ratelimit-Unified-7d_oi-Status: rejected`). The replacement owns the
  whole session. Generic 429s, `Retry-After` alone, and repeated temporary failures
  never authorize switching. Other providers need an explicit quota classification
  from their handlers.
- **Both accounts exhausted does not cause endless retries.** Cooldowns exclude
  exhausted accounts. `routing.retry.request-retry` bounds additional rounds;
  `routing.retry.max-retry-interval` caps each wait, not total request duration.
  Once the budget or wait limit is reached, the proxy returns 429. Requests during
  cooldown can fail locally without calling upstream. Clients should honor
  `Retry-After` when present, otherwise use exponential backoff with jitter.
  Explicit Fable model reset deadlines are honored; missing applicable deadlines
  retain the existing exponential cooldown (up to 30 minutes).
- **Availability may decrease intentionally.** A missing, disabled, or
  model-incompatible owner returns an error instead of switching. Keep credential
  IDs stable. If no eligible replacement exists after exhaustion, the old binding
  remains.
- **Bindings never expire.** `session-affinity-ttl` does not expire durable
  ownership. Disabling affinity restores ordinary routing but retains the file;
  re-enabling it restores saved owners.
- **One process and one SDK Manager per file.** No shared-file coordination exists
  across replicas. Each ownership or alias change rewrites the complete JSON map;
  this design targets modest single-process deployments.
- **Storage errors fail closed.** Read, format, or write errors stop durable
  routing. Repair storage and restart. Do not edit the file while the proxy runs.
  Deleting it resets ownership and permits new account choices; back it up first.
- **Unsupported routing modes:** Home, scheduler plugins, and routes containing
  multiple actual providers. Ordinary proxy routing with one actual provider is
  supported, including its internal `mixed` selection path. Antigravity credits
  fallback is disabled in durable mode because it bypasses normal selection.

## Reading affinity logs

At info level, look for `durable binding created` and `durable binding migrated`.
These events are logged only after persistence succeeds. Migration includes the
previous and replacement credential IDs, requested model, quota reason
(`credential_quota` or `model_quota`), and quota reset deadline in UTC.

The `binding` field is a 12-character prefix of the persisted binding hash, shared
by aliases and stable across restarts. It does not expose the raw session ID.
Enable debug logging to see `durable binding retained` and memory-cache events.
A memory-cache miss does not mean the persisted binding changed. Failed selection
or persistence does not produce a successful migration event.

## SDK lookup caveat

`Manager.LookupSessionAffinity(provider, model, sessionID)` is a read-only Go API
for software embedding CLIProxyAPI. It still inspects the in-memory affinity cache
and lacks the caller scope needed to inspect durable ownership reliably.

This limitation does **not** affect normal HTTP proxy routing or persistence in
the Docker deployment above. Routing already uses the durable store. No action is
needed unless an embedded integration calls this inspection API. A future fix
would add a caller-scoped durable lookup that resolves aliases without selecting
or migrating an account.

See [Durable session affinity](docs/durable-session-affinity.md) for implementation
details and selective binding recovery instructions.
