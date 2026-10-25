#!/usr/bin/env bash
# Ephemeral native upstream CI only. Never change host-wide restrictions.
set -euo pipefail
export LC_ALL=C
fail() {
  printf "[rcp-apparmor] assertion failed at line %s\n" "${BASH_LINENO[0]}" >&2
  exit 1
}
mode=${1:-}
[[ "$mode" = setup || "$mode" = cleanup ]] || fail
[[ ${CI:-} = true && ${GITHUB_ACTIONS:-} = true ]] || fail
[[ ${GITHUB_REPOSITORY:-} = deadnodes/ai-development-workspace ]] || fail
[[ ${GITHUB_EVENT_NAME:-} = push || ${GITHUB_EVENT_NAME:-} = workflow_dispatch ]] || fail
[[ -n ${GITHUB_ACTOR:-} && ${GITHUB_ACTOR} != *'[bot]' && ${GITHUB_ACTOR} != renovate ]] || fail
[[ ${GITHUB_RUN_ID:-} =~ ^[0-9]+$ && ${GITHUB_RUN_ATTEMPT:-} =~ ^[0-9]+$ ]] || fail
[[ ${RUNNER_OS:-}/${RUNNER_ARCH:-} = Linux/X64 && $(uname -s)/$(uname -m) = Linux/x86_64 ]] || fail
[[ $(id -u) != 0 && ${RUNNER_TEMP:-} = /* && -d $RUNNER_TEMP && ! -L $RUNNER_TEMP ]] || fail
command -v apparmor_parser >/dev/null || fail
profile="rcp-native-rootlesskit-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
state="$RUNNER_TEMP/$profile"
restriction=/proc/sys/kernel/apparmor_restrict_unprivileged_userns
unconfined=/proc/sys/kernel/apparmor_restrict_unprivileged_unconfined
negative_control() {
  local output status=0
  # Ubuntu allows a bare user namespace but denies capabilities inside it.
  # Exercise CAP_SYS_ADMIN in a mapped-root user namespace, as rootless mount
  # setup requires; a bare unshare -U true is not a valid negative control.
  output=$(timeout 10 unshare -Ur -m true 2>&1) || status=$?
  printf "[rcp-apparmor] unrelated mapped-root/mount probe exit=%s\n" "$status"
  [[ $status != 0 && $status != 124 ]] || fail
  [[ "$output" = *'Operation not permitted'* || "$output" = *'Permission denied'* ]] || fail
  printf 'Unrelated host mapped-root/mount namespace denied: %s\n' "$output"
}
assert_restrictions() {
  [[ $(cat "$restriction") = 1 ]] || fail
  [[ $(cat "$unconfined") = "$(cat "$state/unconfined")" ]] || fail
}
policy() {
  printf 'abi <abi/4.0>,\ninclude <tunables/global>\nprofile %s /usr/bin/rootlesskit flags=(unconfined) {\n  userns,\n}\n' "$profile"
}
if [[ "$mode" = setup ]]; then
  [[ $(cat /sys/module/apparmor/parameters/enabled) = Y ]] || fail
  [[ $(cat "$restriction") = 1 ]] || fail
  [[ $(cat "$unconfined") =~ ^[01]$ ]] || fail
  negative_control
  # Unique state/profile; refuse any existing RootlessKit attachment rather than replace it.
  [[ ! -e "$state" && ! -L "$state" ]] || fail
  profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
  rootless_profiles=$(printf '%s\n' "$profiles" | grep -i rootlesskit || true)
  if [[ "$rootless_profiles" = 'rootlesskit (unconfined)' ]]; then
    # Reuse the distribution executable attachment, never select it for a container.
    [[ -f /etc/apparmor.d/rootlesskit && ! -L /etc/apparmor.d/rootlesskit ]] || fail
    grep -Eq '^profile[[:space:]]+rootlesskit[[:space:]]+/usr/bin/rootlesskit[[:space:]]+flags=\(unconfined\)[[:space:]]*\{' /etc/apparmor.d/rootlesskit || fail
    [[ $(apparmor_parser -N /etc/apparmor.d/rootlesskit) = rootlesskit ]] || fail
    mkdir -m 700 "$state"
    cat "$unconfined" > "$state/unconfined"
    printf '%s\n' rootlesskit > "$state/reused"
    assert_restrictions
    negative_control
    printf 'RCP_TEST_APPARMOR_PROFILE=rootlesskit\n' >> "${GITHUB_ENV:?}"
    exit 0
  fi
  if [[ -n "$rootless_profiles" ]]; then
    echo 'Existing RootlessKit profile requires explicit inspection; refusing overlap.' >&2
    exit 1
  fi
  mkdir -m 700 "$state"
  cat "$unconfined" > "$state/unconfined"
  policy > "$state/profile"
  sudo apparmor_parser -a "$state/profile"
  profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
  [[ "$profiles" = *"$profile (unconfined)"* ]] || fail
  assert_restrictions
  negative_control
  printf 'RCP_TEST_APPARMOR_PROFILE=%s\n' "$profile" >> "${GITHUB_ENV:?}"
else
  # Setup may have failed before creating state. Nothing was loaded in that case.
  if [[ -e "$state" || -L "$state" ]]; then
    [[ -d "$state" && ! -L "$state" ]] || fail
    if [[ -e "$state/reused" || -L "$state/reused" ]]; then
      [[ -f "$state/reused" && ! -L "$state/reused" && ! -e "$state/profile" && ! -L "$state/profile" ]] || fail
      [[ $(cat "$state/reused") = rootlesskit ]] || fail
      profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
      printf '%s\n' "$profiles" | grep -Fxq 'rootlesskit (unconfined)' || fail
      # Existing profile is not ours to unload.
    else
      [[ ! -L "$state/profile" && -f "$state/profile" ]] || fail
      cmp -s "$state/profile" <(policy) || fail
      profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
      if [[ "$profiles" = *"$profile ("* ]]; then
        sudo apparmor_parser -R "$state/profile"
      fi
      profiles=$(sudo cat /sys/kernel/security/apparmor/profiles)
      [[ "$profiles" != *"$profile ("* ]] || fail
    fi
    # Policy drift must not prevent removal of our exact temporary profile.
    [[ -f "$state/unconfined" && ! -L "$state/unconfined" ]] || fail
    [[ $(cat "$state/unconfined") =~ ^[01]$ ]] || fail
    assert_restrictions
  fi
  [[ $(cat /sys/module/apparmor/parameters/enabled) = Y ]] || fail
  [[ $(cat "$restriction") = 1 ]] || fail
  [[ $(cat "$unconfined") =~ ^[01]$ ]] || fail
  negative_control
fi
