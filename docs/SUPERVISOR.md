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
  "submit_delay": "800ms",
  "idle_rate_limit": "15m",
  "owner_blocked_rate_limit": "4h",
  "active_markers": ["esc to interrupt", "background task", "bashes running", "agents running", "shells running", "Working ("],
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

Each human runs their own supervisor with their own agent token on a host and
tmux server they own. A registered wake target is trusted only within that
supervisor's own tmux server. Do not share a tmux server between humans.
Event polling and delivery are filtered by that human's
Integration assignments. `work_available` events are visible and deliverable
only to the registered user or an agent owned by that user. A service principal
can still see and deliver existing Integration events across assignments.
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
executor harnesses on execution claims are for display and turn-end observation,
never wake targets. Keep the orchestrator in that pane
and attach to the same session from another Mac:

Adding unassigned work to a group assigns it to the group's human owner in the
same transaction. Work assigned to another user cannot be added; group turn-end
and wake routing skip members whose current assignee is not the group owner.

For work without a group orchestrator harness, also use `register_user_orchestrator`
with the same harness. Use a Product-specific registration or omit the Product
for a single registration across the caller's visible Products. `work_available`
wakes the session with a fixed prompt to read `next_work` and the question inbox.
A question continuation also points to the event's triggering `question_ids`.
No question text is sent to the terminal by the supervisor. Replacing a group
harness or removing a member retires pending blocker wakes for the old target
and reroutes them to the current eligible target.

```sh
ssh -t <host> tmux attach -t orch-backend
```

The supervisor resolves the registered target to a local tmux session and pane
ID before delivery. Captures, paste, and Enter all use that pane ID even if the
active pane changes. Rate limits use the tmux session ID, not the target alias.
The supervisor waits for stable pane contents and checks the last 15 lines for
the interactive busy marker before sending a literal message and Enter. It
waits `submit_delay` (default `800ms`) after literal input and before sending
Enter. A delivery is accepted only after observing the case insensitive
`esc to interrupt` marker within `verify_for`. For ordinary continuation wakes,
if the marker does not appear within three seconds, it sends Enter once more and
continues verification. If
the last eight pane lines already contain `[RCP supervisor]`, it sends Enter
without typing another copy, to submit a previous pending wake. Otherwise it
records `wake not observed`. A busy pane is acknowledged `failed` with detail
`busy`.

### Idle and stuck orchestrator wakes

The five `group_*` idle events and legacy `execution_stalled` use the same
active-pane guard, separate from explicit turn-end continuation. When stall
and member staleness overlap, the supervisor uses one guarded wake. Before
typing, the supervisor checks the last 15 lines of two pane captures for the
case-insensitive busy marker and configured `active_markers`. Changing output,
active background tasks, and panes displaying a marker are left alone. The
supervisor ACKs `skipped_active`; RCP records the skip without escalation and
retries after the product's `idle_after` (10 minutes if that check is disabled),
provided the condition still holds. Heartbeats defer group inactivity checks,
but not per-member staleness. Accepted progress refreshes both checks even
for waiting or unclaimed members. `group_member_stale` detects unfinished members
with no `record_progress` for 15 minutes by default; the Product policy can
override that interval or set `member_stale_after_seconds` to zero to disable it.

Idle, wait-timeout, actionable-member, stalled and stale-member wakes share a per-session limit of
one attempt per 15 minutes (`idle_rate_limit`). Owner reminders use their own
per-session limit of one per four hours (`owner_blocked_rate_limit`). Both
timestamps survive a supervisor restart in `state_file`; an active skip does
not use the local rate limit. Locally rate-limited events are left unclaimed.

These wakes require a registered tmux pane with bracketed paste enabled; there
is no headless fallback. The fixed prompt is at most 12 lines and includes only
validated IDs, states, progress ages and next-action codes, never titles or
agent text. It tells the orchestrator to reload the group; an owner reminder
explicitly tells it not to resume work. The supervisor does not retry Enter for
these multiline prompts. Legacy continuation still follows its existing rules.
If RCP supplies a configured orchestrator skill, the prompt includes the exact
`get_agent_skill` call and asks the orchestrator to check and wake its stuck
executors. An owner reminder still forbids waking owner-blocked work.

On the orchestrator host, install the connection with
`release-control connect --harness both --codex-notify --codex-auto-approve --profile default`.
When running Codex TUI with `-a never`, configure the RCP server under
`mcp_servers.<name>.default_tools_approval_mode = "approve"`; without this,
every MCP call fails with “requires approval, but approval policy is never”.
On first start in a new directory, Codex may also ask to trust the folder and
approve hooks. Complete those prompts before expecting supervisor wakes to run.

## Turn-end hooks

Turn-end hooks continue to report `report_turn_end` to the server with the
registered `harness_kind`, exact `session_ref`, and turn ID when available.
Claude Code's `Stop` hook and Codex's `agent-turn-complete` notify event are the
expected sources. The server applies its debounce and emits
`execution_continuation_required`; the supervisor only handles the resulting
durable event. The supervisor batches by the server-computed `wake_target`.
Every member event, including stalled and turn-end continuation, targets the
first open containing group with a registered orchestrator harness. Without one,
RCP uses the assignee's Product-specific or global user orchestrator; if neither
exists, the event escalates with no target and the supervisor ACKs unsupported.
The supervisor never wakes an executor. `verify_for` limits post-send verification
(default: `wake_timeout`); the whole wake is bounded by `wake_timeout`.

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
  "submit_delay": "800ms",
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

Have the orchestrator register the group's `orchestrator_harness` with
`tmux_target` set to the exact pane `orch-backend:0.1` through `create_execution_group` or
`set_execution_group_harness`. It claims each member separately with
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

### Limits

Register an orchestrator harness with an exact pane target, either
`session:window.pane` (for example `orch-backend:0.1`) or `%pane_id`. A bare
session target resolves to that session's active pane when delivery occurs, so
pane focus changes can redirect a wake. Run one orchestrator per tmux session;
cooldowns are tracked per session. Headless resume is not used for orchestrator
wakes: every orchestrator must have a tmux target.

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

# Harness turn-end hook

The optional `scripts/hooks/rcp-turn-end.sh` reports completed agent turns to
`POST /api/commands`. Configure `rcp_url`, `token_file`, `product_id`, and
`harness_kind` (`codex` or `claude_code`) in JSON. Codex accepts a config path
before its appended JSON payload:

```sh
codex -c 'notify=["/path/rcp-turn-end.sh", "--config", "/path/turn-end.json"]'
```

Without `--config`, it reads `${RCP_HOOK_CONFIG:-$HOME/.config/release-control/turn-end.json}`.
The `RCP_URL`, `RCP_TOKEN_FILE`, `RCP_PRODUCT_ID`, and `RCP_HARNESS_KIND`
environment variables override config values. The token file should have mode
`600`. Codex's `thread-id` is read from the notify JSON argument; Claude Code's
`session_id` is read from Stop-hook JSON on stdin. If `$TMUX` is set, it also
reports the current tmux session name. Failures are logged to `$RCP_HOOK_LOG`,
defaulting to `~/Library/Logs/rcp-turn-end.log`, and do not interrupt the agent.

Codex config example:

```toml
notify = ["/path/rcp-turn-end.sh", "--config", "/path/turn-end.json"]
```

For Claude Code, add a Stop hook to `.claude/settings.json`:

```json
{
  "hooks": {
    "Stop": [{
      "hooks": [{ "type": "command", "command": "/path/to/rcp-turn-end.sh" }]
    }]
  }
}
```

Codex notify reports are ignored unless their `type` is `agent-turn-complete`.


Execution completion means each Integration reaches its `desired_state` and then
`release_execution(outcome=completed)` is recorded by the orchestrator. After
idle expiry, the orchestrator checks its workers and handles any lease renewal
or takeover before dispatching more work.
Every fenced progress write and transition must include `execution_fence`.
