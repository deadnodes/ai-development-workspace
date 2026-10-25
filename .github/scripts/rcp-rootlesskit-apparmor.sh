#!/usr/bin/env bash
# Ephemeral native upstream CI only. Never change host-wide restrictions.
set -euo pipefail
export LC_ALL=C
mode=${1:-}
[[ "$mode" = setup || "$mode" = cleanup ]] || exit 1
[[ ${CI:-} = true && ${GITHUB_ACTIONS:-} = true ]] || exit 1
[[ ${GITHUB_REPOSITORY:-} = deadnodes/ai-development-workspace ]] || exit 1
[[ ${GITHUB_EVENT_NAME:-} = push || ${GITHUB_EVENT_NAME:-} = workflow_dispatch ]] || exit 1
[[ -n ${GITHUB_ACTOR:-} && ${GITHUB_ACTOR} != *'[bot]' && ${GITHUB_ACTOR} != renovate ]] || exit 1
[[ ${GITHUB_RUN_ID:-} =~ ^[0-9]+$ && ${GITHUB_RUN_ATTEMPT:-} =~ ^[0-9]+$ ]] || exit 1
[[ ${RUNNER_OS:-}/${RUNNER_ARCH:-} = Linux/X64 && $(uname -s)/$(uname -m) = Linux/x86_64 ]] || exit 1
[[ $(id -u) != 0 && ${RUNNER_TEMP:-} = /* && -d $RUNNER_TEMP && ! -L $RUNNER_TEMP ]] || exit 1
command -v apparmor_parser >/dev/null
profile="rcp-native-rootlesskit-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
state="$RUNNER_TEMP/$profile"
restriction=/proc/sys/kernel/apparmor_restrict_unprivileged_userns
unconfined=/proc/sys/kernel/apparmor_restrict_unprivileged_unconfined
negative_control() {
  local output status=0
  output=$(timeout 10 unshare -U true 2>&1) || status=$?
  [[ $status != 0 && $status != 124 ]] || exit 1
  [[ "$output" = *'Operation not permitted'* || "$output" = *'Permission denied'* ]] || exit 1
  printf 'Unrelated host userns denied: %s\n' "$output"
}
assert_restrictions() {
  [[ $(cat "$restriction") = 1 ]] || exit 1
  [[ $(cat "$unconfined") = "$(cat "$state/unconfined")" ]] || exit 1
}
policy() {
  printf 'abi <abi/4.0>,\ninclude <tunables/global>\nprofile %s /usr/bin/rootlesskit flags=(unconfined) {\n  userns,\n}\n' "$profile"
}
if [[ "$mode" = setup ]]; then
  [[ $(cat /sys/module/apparmor/parameters/enabled) = Y ]] || exit 1
  [[ $(cat "$restriction") = 1 ]] || exit 1
  [[ $(cat "$unconfined") =~ ^[01]$ ]] || exit 1
  negative_control
  # Unique state/profile; refuse any existing RootlessKit attachment rather than replace it.
  [[ ! -e "$state" && ! -L "$state" ]] || exit 1
  profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
  if [[ "${profiles,,}" = *rootlesskit* ]]; then
    echo 'Existing RootlessKit profile requires explicit inspection; refusing overlap.' >&2
    exit 1
  fi
  mkdir -m 700 "$state"
  cat "$unconfined" > "$state/unconfined"
  policy > "$state/profile"
  sudo apparmor_parser -a "$state/profile"
  profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
  [[ "$profiles" = *"$profile (unconfined)"* ]] || exit 1
  assert_restrictions
  negative_control
  printf 'RCP_TEST_APPARMOR_PROFILE=%s\n' "$profile" >> "${GITHUB_ENV:?}"
else
  # Setup may have failed before creating state. Nothing was loaded in that case.
  if [[ -e "$state" || -L "$state" ]]; then
    [[ -d "$state" && ! -L "$state" && ! -L "$state/profile" && -f "$state/profile" ]] || exit 1
    cmp -s "$state/profile" <(policy) || exit 1
    profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
    if [[ "$profiles" = *"$profile ("* ]]; then
      sudo apparmor_parser -R "$state/profile"
    fi
    profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
    [[ "$profiles" != *"$profile ("* ]] || exit 1
    # Policy drift must not prevent removal of our exact temporary profile.
    [[ -f "$state/unconfined" && ! -L "$state/unconfined" ]] || exit 1
    [[ $(cat "$state/unconfined") =~ ^[01]$ ]] || exit 1
    assert_restrictions
  fi
  [[ $(cat /sys/module/apparmor/parameters/enabled) = Y ]] || exit 1
  [[ $(cat "$restriction") = 1 ]] || exit 1
  [[ $(cat "$unconfined") =~ ^[01]$ ]] || exit 1
  negative_control
fi
