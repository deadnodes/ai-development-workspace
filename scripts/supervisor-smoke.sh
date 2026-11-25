#!/bin/sh
set -eu

target=${1:?usage: supervisor-smoke.sh <tmux-target>}
case "$target" in
	*[!A-Za-z0-9_.:-]*|'') echo "invalid tmux target" >&2; exit 2 ;;
esac
if [ "${#target}" -gt 64 ]; then echo "invalid tmux target" >&2; exit 2; fi
tmux has-session -t "$target"
echo "--- pane before ---"
tmux capture-pane -p -t "$target"
tmux send-keys -t "$target" -l '[RCP supervisor smoke test] If this message appears, reply SMOKE_OK.'
tmux send-keys -t "$target" Enter
echo "--- pane after ---"
sleep 1
tmux capture-pane -p -t "$target"
