#!/bin/sh

log_file=${RCP_HOOK_LOG:-"$HOME/Library/Logs/rcp-turn-end.log"}
log() {
  mkdir -p "$(dirname "$log_file")" 2>/dev/null || :
  printf '%s\n' "rcp-turn-end: $1" >>"$log_file" 2>/dev/null || :
}
fail() { log "$1"; exit 0; }

[ -n "${RCP_URL:-}" ] || fail 'missing RCP_URL'
[ -n "${RCP_TOKEN_FILE:-}" ] || fail 'missing RCP_TOKEN_FILE'
[ -n "${RCP_PRODUCT_ID:-}" ] || fail 'missing RCP_PRODUCT_ID'
case "${RCP_HARNESS_KIND:-}" in codex|claude_code) ;; *) fail 'invalid RCP_HARNESS_KIND' ;; esac
[ -r "$RCP_TOKEN_FILE" ] || fail 'token file is not readable'
token=$(cat "$RCP_TOKEN_FILE" 2>/dev/null) || fail 'could not read token file'
[ -n "$token" ] || fail 'token file is empty'

payload=
if [ "$RCP_HARNESS_KIND" = codex ]; then
  payload=${1:-}
else
  payload=$(cat 2>/dev/null) || payload=
fi
session_ref=
if command -v python3 >/dev/null 2>&1 && [ -n "$payload" ]; then
  if [ "$RCP_HARNESS_KIND" = codex ]; then
    parsed=$(printf '%s' "$payload" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("\t" + str(d.get("thread-id", "")) if d.get("type") == "agent-turn-complete" else "\tIGNORE")' 2>/dev/null) || parsed=
  else
    parsed=$(printf '%s' "$payload" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("\t" + str(d.get("session_id", "")))' 2>/dev/null) || parsed=
  fi
  case "$parsed" in *IGNORE) exit 0 ;; esac
  session_ref=${parsed#$(printf '\t')}
fi
tmux_target=
if [ -n "${TMUX:-}" ] && command -v tmux >/dev/null 2>&1; then
  tmux_target=$(tmux display-message -p '#S' 2>/dev/null) || tmux_target=
fi
[ -n "$session_ref" ] || [ -n "$tmux_target" ] || fail 'no session_ref or tmux_target available'

body=$(RCP_SESSION_REF="$session_ref" RCP_TMUX_TARGET="$tmux_target" RCP_PRODUCT_ID="$RCP_PRODUCT_ID" RCP_HARNESS_KIND="$RCP_HARNESS_KIND" python3 -c 'import json,os; d={"action":"report_turn_end","actor":"agent/harness-hook","product_id":os.environ["RCP_PRODUCT_ID"],"data":{"harness_kind":os.environ["RCP_HARNESS_KIND"]}}; s=os.environ.get("RCP_SESSION_REF"); t=os.environ.get("RCP_TMUX_TARGET"); d["data"].update(({"session_ref":s} if s else {})); d["data"].update(({"tmux_target":t} if t else {})); print(json.dumps(d))' 2>/dev/null) || fail 'python3 is required to encode request'
if command -v curl >/dev/null 2>&1 && curl -fsS --max-time 5 -X POST "${RCP_URL%/}/api/commands" -H 'Content-Type: application/json' -H "Authorization: Bearer $token" --data-binary "$body" >/dev/null 2>&1; then
  log 'reported turn end'
else
  fail 'request failed'
fi
exit 0
