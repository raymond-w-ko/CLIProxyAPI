# Fork notes: durable session affinity

This fork keeps each session bound to one account across model changes, config
reloads, and restarts. Only confirmed account-wide quota exhaustion permits a
switch. Temporary errors, repeated failures, and generic HTTP 429 responses keep
the binding. The existing retry budgets, refresh, cooldowns, and wait limits still
apply; this patch adds no retry loops or network timeouts.

After a permitted switch, the replacement remains the owner even when the old
account recovers. Account affinity reduces unnecessary Claude thinking loss, but
cannot transfer thinking blocks between unrelated accounts.

## Recommended configuration

Merge these settings into your existing `config.yaml`. Preserve the other settings
and do not create duplicate `routing` or `oauth` sections.

```yaml
routing:
  session-affinity: true
  session-affinity-subagents: true
  cooldown:
    disable-cooling: false
    save-cooldown-status: true

oauth:
  auth-dir: /root/.cli-proxy-api
  providers:
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
  Claude's `model-level-cooling` must remain `false` so confirmed account-wide
  exhaustion can authorize migration.

No `session-affinity-file` setting is needed. The file is always
`session-bindings.json` beside the active config file, including with `--config`.
Management page saves of the YAML do not modify this file.

## Recommended Docker Compose deployment

Use an image built from this fork's patched source. Upstream
`eceasy/cli-proxy-api:latest` does not automatically include these changes.

For a local image, run this from the patched checkout:

```bash
docker build -t cliproxyapi-affinity:local .
```

Save this as your deployment's `compose.yaml`. Replace the image tag if you publish
or obtain a different image containing the patch.

```yaml
services:
  cliproxyapi:
    image: cliproxyapi-affinity:local
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
docker compose -f compose.yaml up -d
```

Bindings then persist at `./config/session-bindings.json` on the host. Preserve
both `./config/` and `./auths/` across container replacements and backups.

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
  header classification to establish an active account-wide quota cooldown.
  Other providers need the same account-wide state from their existing handlers.
  Repeated temporary failures never authorize switching.
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
