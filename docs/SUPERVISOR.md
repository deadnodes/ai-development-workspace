# RCP supervisor (macOS)

The supervisor is a separate process from the Release Control server. Run it on
the Mac mini that owns the interactive Claude Code or Codex sessions. It polls
the configured Product event queues, claims due deliveries, wakes the exact
registered session once per cycle, and records the adapter outcome.

## Build and configure

Build with `make build` or directly with `go build -o ~/bin/rcp-supervisor ./cmd/supervisor`.
Create `~/.config/release-control/supervisor.json` with mode `0600`:

```json
{
  "rcp_url": "https://rcp.example.com",
  "profile": "default",
  "supervisor_id": "mini-orchestrator",
  "product_ids": ["PRODUCT_ID"],
  "poll_interval": "15s",
  "idle_window": "5s",
  "wake_timeout": "120s",
  "verify_for": "5s",
  "headless_kinds": [],
  "allowed_kinds": ["codex", "claude_code", "claude_desktop", "ccr", "other"]
}
```

Set `token_env` instead of `profile` only when a separately managed environment
variable supplies the bearer token. Do not place token values in this file.
With `profile`, the supervisor resolves the same encrypted credential managed by
`release-control login`; it does not print or persist the token. Optional
`rate_limit` overrides the default of six idle windows per session. Optional
`state_file` selects where the non-secret last-wake timestamps are stored. The
`tmux_path`, `codex_path`, `claude_path`, and `pgrep_path` fields can pin command
paths. Defaults are resolved using the launchd `PATH`.
`headless_kinds` defaults to empty, so only registered tmux sessions are woken.
Opt in per kind (currently `codex` and `claude_code`) only when headless resume is
intended; the process-list guard cannot see an interactive TUI whose session ID
is absent from its argv, so enabling headless can start a second session.

Run once in a terminal with `~/bin/rcp-supervisor -config ~/.config/release-control/supervisor.json`.
The process handles SIGTERM and Ctrl-C. It continues polling after transient
HTTP errors.

## tmux session convention

Use one named tmux session for each orchestrator, for example `orch-backend` or
`orch-platform`. Register the orchestrator harness on the execution group at
creation (`orchestrator_harness`) or with `set_execution_group_harness`. The
registration contains `kind` and `tmux_target` (and `session_ref` when available).
Re-register it after every restart that creates a new harness session. Member
executor harnesses belong on execution claims. Keep the orchestrator in that pane
and attach to the same session from another Mac:

```sh
ssh -t <host> tmux attach -t orch-backend
```

The supervisor waits for stable pane contents and checks the last 15 lines for
the interactive busy marker before sending a literal message and Enter. It
acknowledges `delivered` only after seeing “esc to interrupt” or when the typed
message has disappeared from the last five lines. Otherwise it records
`wake not observed`. A busy pane is acknowledged `failed` with detail `busy`.

## Turn-end hooks

Turn-end hooks continue to report `report_turn_end` to the server with the
registered `harness_kind`, exact `session_ref`, and turn ID when available.
Claude Code's `Stop` hook and Codex's `agent-turn-complete` notify event are the
expected sources. The server applies its debounce and emits
`execution_continuation_required`; the supervisor only handles the resulting
durable event. Each event has a server-computed `wake_target`. The supervisor batches by that target: running/stalled work uses its executor harness; unassigned, lost, exited-incomplete, continuation-required, and newly actionable waiting/blocked work uses the first open containing group’s registered orchestrator harness. Without one, the supervisor ACKs unsupported and RCP marks escalation. `verify_for` limits post-send verification (default: `wake_timeout`); process spawning and the complete wake remain bounded by `wake_timeout`.

## Mac mini installation runbook

On the mini, from a checkout of this repository, build and configure the agent:

```sh
mkdir -p ~/bin ~/.config/release-control ~/Library/Logs ~/Library/LaunchAgents
go build -o ~/bin/rcp-supervisor ./cmd/supervisor
chmod 700 ~/bin/rcp-supervisor
cat > ~/.config/release-control/supervisor.json <<'JSON'
{
  "rcp_url": "https://rcp.example.com",
  "profile": "default",
  "supervisor_id": "mini-orchestrator",
  "product_ids": ["PRODUCT_ID"],
  "poll_interval": "15s",
  "idle_window": "5s",
  "wake_timeout": "120s",
  "verify_for": "5s",
  "allowed_kinds": ["codex", "claude_code"],
  "headless_kinds": []
}
JSON
chmod 600 ~/.config/release-control/supervisor.json
```

Log in once with the installed CLI (`release-control login --url
https://rcp.example.com --profile default`) and complete its browser approval.
Start the orchestrator in its persistent tmux pane, for example:

```sh
tmux new -s orch-backend
```

Have the orchestrator register its harness with `tmux_target` set to
`orch-backend` (or the precise pane such as `orch-backend:0.1`) on
`claim_execution`. Install the provided plist, replacing `OWNER` with the
mini's short account name:

```sh
cp deploy/launchd/com.deadnodes.rcp-supervisor.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.deadnodes.rcp-supervisor.plist
launchctl kickstart -k gui/$(id -u)/com.deadnodes.rcp-supervisor
tail -f ~/Library/Logs/rcp-supervisor.log
```

From a MacBook, attach to the same orchestrator session:

```sh
ssh -t <mini-host> tmux attach -t orch-backend
```

Run a harmless delivery smoke check on the mini with
`scripts/supervisor-smoke.sh orch-backend:0.1`.

## launchd

Copy `deploy/launchd/com.deadnodes.rcp-supervisor.plist` to
`~/Library/LaunchAgents/`, replace `OWNER` with the account's short home-folder
name, create `~/Library/Logs`, and load it:

```sh
mkdir -p ~/Library/LaunchAgents ~/Library/Logs
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.deadnodes.rcp-supervisor.plist
launchctl kickstart -k gui/$(id -u)/com.deadnodes.rcp-supervisor
```

Logs go to `~/Library/Logs/rcp-supervisor.log`. The plist `PATH` includes
`/opt/homebrew/bin` and `~/.local/bin`. To unload:

```sh
launchctl bootout gui/$(id -u)/com.deadnodes.rcp-supervisor
```

## Harness support and limits

| Registered kind | Wake path | Limit |
|---|---|---|
| `codex` | tmux; optional headless `codex exec resume <session_ref> <message>` | Headless requires `headless_kinds: ["codex"]`; process guard is best effort. |
| `claude_code` | tmux; optional headless `claude -p --resume <session_ref> <message>` | Requires `headless_kinds: ["claude_code"]` and CLI auth available to launchd. |
| `claude_desktop` | tmux only | No supported out-of-process Desktop API; without tmux this is acknowledged unsupported. |
| `ccr` | tmux only | No generic router resume protocol is assumed. |
| `other` | tmux only | Adapter-specific wake protocol is not assumed. |

The supervisor never scrapes agent session stores. Session refs must be
registered by their owning orchestrator. tmux detection is pane-content based;
it cannot prove semantic readiness. The headless process guard is a best-effort
process-list check, not an OS lock. `pgrep -f <session_ref>` cannot see an
interactive TUI started without the ID in argv. Keep `headless_kinds` empty on
machines hosting interactive sessions to prevent a second copy. ACK records delivery outcome; the RCP event
remains unresolved until its cause clears.

## Manual smoke test

On the Mac mini, run `scripts/supervisor-smoke.sh orch-backend:0.1`. The script
prints the pane before and after sending a harmless prompt. It does not call RCP
or use a configured supervisor credential.
