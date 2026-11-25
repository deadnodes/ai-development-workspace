#!/bin/sh

log_file=${RCP_HOOK_LOG:-"$HOME/Library/Logs/rcp-turn-end.log"}
log() {
  mkdir -p "$(dirname "$log_file")" 2>/dev/null || :
  printf '%s\n' "rcp-turn-end: $1" >>"$log_file" 2>/dev/null || :
}
fail() { log "$1"; exit 0; }

config=${RCP_HOOK_CONFIG:-"$HOME/.config/release-control/turn-end.json"}
if [ "${1:-}" = --config ]; then
  [ -n "${2:-}" ] || fail 'missing config path'
  config=$2
  shift 2
fi
config_values=
if [ -r "$config" ]; then
  config_values=$(python3 - "$config" <<'PYCONFIG'
import json,sys
try:
 d=json.load(open(sys.argv[1]))
 for k in ('rcp_url','token_file','profile','executable','token_env','product_id','harness_kind'):
  v=d.get(k,'')
  if not isinstance(v,str) or any(c in v for c in '\t\r\n'):
   raise ValueError('invalid '+k)
  print(v)
except Exception:
 sys.exit(1)
PYCONFIG
  ) || fail 'invalid hook config'
elif [ -z "${RCP_URL:-}" ] || [ -z "${RCP_TOKEN_FILE:-}" ] || [ -z "${RCP_PRODUCT_ID:-}" ] || [ -z "${RCP_HARNESS_KIND:-}" ]; then
  fail 'hook config is not readable'
fi
RCP_URL=${RCP_URL:-$(printf '%s\n' "$config_values" | sed -n '1p')}
RCP_TOKEN_FILE=${RCP_TOKEN_FILE:-$(printf '%s\n' "$config_values" | sed -n '2p')}
RCP_PROFILE=${RCP_PROFILE:-$(printf '%s\n' "$config_values" | sed -n '3p')}
RCP_EXECUTABLE=${RCP_EXECUTABLE:-$(printf '%s\n' "$config_values" | sed -n '4p')}
RCP_TOKEN_ENV=${RCP_TOKEN_ENV:-$(printf '%s\n' "$config_values" | sed -n '5p')}
RCP_PRODUCT_ID=${RCP_PRODUCT_ID:-$(printf '%s\n' "$config_values" | sed -n '6p')}
RCP_HARNESS_KIND=${RCP_HARNESS_KIND:-$(printf '%s\n' "$config_values" | sed -n '7p')}
[ -n "$RCP_URL" ] || fail 'missing RCP_URL'
[ -n "$RCP_PRODUCT_ID" ] || fail 'missing RCP_PRODUCT_ID'
case "$RCP_HARNESS_KIND" in codex|claude_code) ;; *) fail 'invalid RCP_HARNESS_KIND' ;; esac
[ -n "$RCP_TOKEN_FILE" ] && { [ -r "$RCP_TOKEN_FILE" ] || fail 'token file is not readable'; token=$(cat "$RCP_TOKEN_FILE" 2>/dev/null) || fail 'could not read token file'; }
[ -z "$RCP_TOKEN_FILE" ] && [ -n "$RCP_PROFILE" ] && { token=$("$RCP_EXECUTABLE" token --url "$RCP_URL" --profile "$RCP_PROFILE" 2>/dev/null) || fail 'saved credential unavailable'; }
[ -z "$RCP_TOKEN_FILE" ] && [ -z "$RCP_PROFILE" ] && [ -n "$RCP_TOKEN_ENV" ] && { token=$(printenv "$RCP_TOKEN_ENV" 2>/dev/null) || fail 'token environment variable unavailable'; }

payload=${1:-}
if [ "$RCP_HARNESS_KIND" != codex ]; then
  payload=$(cat 2>/dev/null) || payload=
fi
session_ref=
if [ -n "$payload" ]; then
  parsed=$(printf '%s' "$payload" | python3 -c 'import json,sys; d=json.load(sys.stdin); typ=d.get("type",""); v=d.get("thread-id","") if typ=="agent-turn-complete" else "IGNORE" if typ else d.get("session_id",""); print(v)' 2>/dev/null) || parsed=
  [ "$parsed" != IGNORE ] || exit 0
  session_ref=$parsed
fi
tmux_target=
if [ -n "${TMUX:-}" ] && command -v tmux >/dev/null 2>&1; then
  tmux_target=$(tmux display-message -p '#S' 2>/dev/null) || tmux_target=
fi
[ -n "$session_ref" ] || [ -n "$tmux_target" ] || fail 'no session_ref or tmux_target available'
body=$(RCP_SESSION_REF="$session_ref" RCP_TMUX_TARGET="$tmux_target" RCP_PRODUCT_ID="$RCP_PRODUCT_ID" RCP_HARNESS_KIND="$RCP_HARNESS_KIND" python3 -c 'import json,os; d={"action":"report_turn_end","actor":"agent/harness-hook","product_id":os.environ["RCP_PRODUCT_ID"],"data":{"harness_kind":os.environ["RCP_HARNESS_KIND"]}}; s=os.environ.get("RCP_SESSION_REF"); t=os.environ.get("RCP_TMUX_TARGET"); d["data"].update(({"session_ref":s} if s else {})); d["data"].update(({"tmux_target":t} if t else {})); print(json.dumps(d))' 2>/dev/null) || fail 'python3 is required to encode request'
auth_args=
if [ -n "$token" ]; then
  header_file=$(mktemp "${TMPDIR:-/tmp}/rcp-auth.XXXXXX") || fail 'could not create authorization header file'
  trap 'rm -f "$header_file"' EXIT HUP INT TERM
  chmod 600 "$header_file" || fail 'could not secure authorization header file'
  printf 'Authorization: Bearer %s\n' "$token" >"$header_file" || fail 'could not write authorization header'
  auth_args=$header_file
fi
unset token
if [ -n "$auth_args" ] && command -v curl >/dev/null 2>&1 && curl -fsS --max-time 5 -X POST "${RCP_URL%/}/api/commands" -H 'Content-Type: application/json' -H "@$auth_args" --data-binary "$body" >/dev/null 2>&1; then
  log 'reported turn end'
elif [ -z "$auth_args" ] && command -v curl >/dev/null 2>&1 && curl -fsS --max-time 5 -X POST "${RCP_URL%/}/api/commands" -H 'Content-Type: application/json' --data-binary "$body" >/dev/null 2>&1; then
  log 'reported turn end'
else
  fail 'request failed'
fi
exit 0
