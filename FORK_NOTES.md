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

Claude's injected date now stays fixed while its in-memory continuity entry
survives, avoiding prompt-cache invalidation at midnight. This date is separate
from durable account ownership: a restart, cache expiry, or account migration
can establish a new date.

An upstream 401 followed by an invalid refresh grant now keeps the credential
unavailable instead of repeatedly retrying it. A new login or a successful
explicit refresh is needed for recovery. Durable affinity still does not migrate
on authentication failure alone. Claude refresh also stops replaying ambiguous
transport or decoding failures because its single-use refresh token may already
have been consumed.

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
