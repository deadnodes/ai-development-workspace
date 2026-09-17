# Service updates — agent runbook

This updates the Control Plane itself, not applications managed by it.
The database is authoritative. Keep the same database, Compose project name,
`.env`, mounts and secret references. Never use `down -v`, reset the database,
force-pull a branch or discard local changes as part of an update.

## Installation record

Keep the following non-secret details in the instance Product's project knowledge
and in the operator's workspace instructions (so recovery does not depend on the service):

- service URL, source checkout, Git remote and tracked branch;
- deployment mode: source Compose, published image, native binary or Kubernetes;
- Compose project and file list, or Kubernetes context/namespace/GitOps mapping;
- data location, backup directory, secret references (never secret values);
- currently installed commit/image digest, previous image, last successful check;
- update owner, schedule and whether automatic application is authorized.

Use one maintenance owner per installation. A normal feature agent should not
restart a shared service independently.

## Check daily, apply when idle

For a source checkout, fetch without changing its working tree:

```sh
git fetch origin main
git status --short
git log --oneline HEAD..origin/main
git rev-list --left-right --count HEAD...origin/main
```

No remote commits means no source update. A dirty checkout or divergent history
needs review; preserve it. Inspect the candidate diff, especially migrations,
configuration, deployment and agent instructions. Require a successful CI run
for the exact candidate commit; a green older run is insufficient.

The repository CI publishes `ghcr.io/<owner>/<repository>` after checks pass on
main or a version tag. Tags include `sha-<full-commit>` and `latest`; deployments
should pin the published digest from the CI summary. Do not infer the running
version from `latest` or from the checkout: inspect the actual container image.
For this repository the package is `ghcr.io/deadnodes/ai-development-workspace`.
This package is public; Docker pulls do not require registry credentials. Registry
login and package access are still separate from the server's GitHub App connection
for private product artifacts. Never place registry credentials in project knowledge.

Before applying:

1. Read `/api/state` (or MCP `get_state`); defer while Operations are PENDING or
   RUNNING. Coordinate a short maintenance window so users/agents do not start
   writes between this check and shutdown.
2. Record maintenance intent and the current image/commit outside the container.
3. Export an application backup using [BACKUP.md](BACKUP.md). Do not print its
   contents. Verify the checksum and retain the file outside the container.
4. Build/pull the candidate before stopping the service.
5. Stop only the app, take the storage backup below, then replace the app.
6. Verify health, `/api/state` and a known Feature through MCP `resume`; compare
   Product/Feature IDs with the pre-update snapshot. Health alone is insufficient.
7. Record the installed image/commit, backup path and verification result. Refresh
   the browser. Re-run `connect` when the agent instruction kit changed.

The service does not update itself. Daily checking/application is performed by an
operator agent or external scheduler, according to this runbook. Do not install
a container auto-updater that bypasses backups and active-operation checks.

## Source Compose installation

Run in the original installation directory. Its ignored `.env` must retain
`COMPOSE_FILE` for all workspace/Kubernetes mounts. Preserve local configuration.
After the preflight above and only with a clean tracked checkout:

```sh
git merge --ff-only origin/main
docker compose --profile app build app
mkdir -p .local/backups
# Record current image ID before replacing it.
docker inspect --format '{{.Image}}' "$(docker compose ps -q app)"
docker compose stop app
# App is stopped: this is the database restore point before new startup/migrations.
backup_file=".local/backups/pre-update-$(date -u +%Y%m%dT%H%M%SZ).dump"
(umask 077; docker compose exec -T db pg_dump -U releasecontrol -d releasecontrol -Fc > "$backup_file")
test -s "$backup_file"
docker compose exec -T db pg_restore --list < "$backup_file" > /dev/null
docker compose --profile app up -d --no-deps --wait --wait-timeout 120 app
curl --fail http://127.0.0.1:8090/healthz
```

Execute steps sequentially and stop on error; do not paste past a failed backup.
Do not prune old images or backups after updating. `restart` alone does not load
new source code or a new image. On failure, inspect logs before retrying.

## Published image installation/update

No Go/Node toolchain is needed. Add `compose.image.yaml` last to your existing
`COMPOSE_FILE` list and set `RCP_IMAGE` in ignored `.env`, for example:

```dotenv
COMPOSE_FILE=compose.yaml:compose.workspace.yaml:compose.image.yaml
RCP_IMAGE=ghcr.io/OWNER/REPOSITORY@sha256:ACTUAL_PUBLISHED_DIGEST
```

Omit `compose.workspace.yaml` when no workspace is mounted. First installation:

```sh
docker compose --profile app pull app
docker compose --profile app up -d --no-build --wait --wait-timeout 120
```

For updates record the old digest, choose the verified new digest, pull it, then
follow the same operation preflight, app stop and PostgreSQL backup sequence.
Start with `up -d --no-build --no-deps --wait --wait-timeout 120 app`. Do not use
`--build` for an image installation. Confirm the container image with
`docker inspect` and then verify the API/MCP data.

## Native binary / embedded storage

Build the candidate separately (`make build`), record the old binary checksum,
export an application backup, stop the service and copy the file at `RCP_DATA_PATH`
to private backup storage. Copy embedded storage only while stopped. Replace the
binary, restart with the same environment/data path, verify health and context.
`DATABASE_URL` installations need a PostgreSQL backup instead. Do not start a
second writer against the same embedded file. There is no published native-binary
release assumed by these instructions.

## Kubernetes / Flux

Use the installation's explicit context and namespace. Record the current image
digest, export state and take an operator-managed PostgreSQL snapshot. Defer active
operations. Update only the Control Plane image digest in its GitOps manifest;
let Flux reconcile. Verify rollout, health and API/MCP context. See
[KUBERNETES.md](KUBERNETES.md). Do not patch managed application workloads or
switch database credentials during a Control Plane update.

## Failure and rollback

Keep the app stopped if the database migration or state verification fails.
An old image is safe only if the database schema is compatible. Otherwise restore
the pre-update database snapshot into a separate database, run the old image
against it and verify before switching clients. Application archives restore only
into an empty instance; they do not overwrite a running installation.
Never automatically restore over a database containing newer user work.

## Refresh installed agent instructions

After upgrading the server, build the native host CLI when needed and run the
same connection command again for each connected workspace:

```sh
./bin/release-control connect --url http://127.0.0.1:8090 \
  --workspace /absolute/path/to/project --product-id PRODUCT_ID
```

Preserve the original URL, product, server name and token-environment option.
The installer refreshes its managed instructions and handoff skill; it should
not replace unrelated workspace instructions. Reload MCP/start a new agent session.
