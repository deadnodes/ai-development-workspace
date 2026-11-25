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

Run once in a terminal with `~/bin/rcp-supervisor -config ~/.config/release-control/supervisor.json`.
The process handles SIGTERM and Ctrl-C. It continues polling after transient
HTTP errors.

## tmux session convention

Use one named tmux session for each orchestrator, for example `orch-backend` or
`orch-platform`. Register the corresponding `tmux_target` on each execution
claim, e.g. `orch-backend` or `orch-backend:0.1`. Keep the orchestrator in that
pane and attach to the same session from another Mac:

```sh
ssh -t <host> tmux attach -t orch-backend
```

The supervisor waits for stable pane contents and checks the last 15 lines for
the interactive busy marker before sending a literal message and Enter. It
verifies that pane contents change before acknowledging `delivered`. A busy
pane is acknowledged `failed` with detail `busy`, allowing server backoff and
redelivery.

## Turn-end hooks

Turn-end hooks continue to report `report_turn_end` to the server with the
registered `harness_kind`, exact `session_ref`, and turn ID when available.
Claude Code's `Stop` hook and Codex's `agent-turn-complete` notify event are the
expected sources. The server applies its debounce and emits
`execution_continuation_required`; the supervisor only handles the resulting
durable event. A tmux target is included in execution events along with harness
kind and session reference.

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
| `codex` | tmux; otherwise `codex exec resume <session_ref> <message>` | Headless resume is skipped if `pgrep -f` finds a live process for the exact session ref. |
| `claude_code` | tmux; otherwise `claude -p --resume <session_ref> <message>` | Requires Claude CLI auth available to launchd; live process guard applies. |
| `claude_desktop` | tmux only | No supported out-of-process Desktop API; without tmux this is acknowledged unsupported. |
| `ccr` | tmux only | No generic router resume protocol is assumed. |
| `other` | tmux only | Adapter-specific wake protocol is not assumed. |

The supervisor never scrapes agent session stores. Session refs must be
registered by their owning orchestrator. tmux detection is pane-content based;
it cannot prove semantic readiness. The headless process guard is a best-effort
process-list check, not an OS lock. ACK records delivery outcome; the RCP event
remains unresolved until its cause clears.

## Manual smoke test

On the Mac mini, run `scripts/supervisor-smoke.sh orch-backend:0.1`. The script
prints the pane before and after sending a harmless prompt. It does not call RCP
or use a configured supervisor credential.
