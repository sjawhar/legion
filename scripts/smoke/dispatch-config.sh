# shellcheck shell=bash
# Sourced by up.sh and checkpoints.sh -- `source "$(dirname "${BASH_SOURCE[0]}")/dispatch-config.sh"`
# -- never executed. The caller defines `fail <message>` (prints and exits nonzero) before calling.
#
# DISPATCH_URL and DISPATCH_TOKEN come from the environment when set (an empty value counts as
# unset); otherwise from `.dispatch.serverUrl` / `.dispatch.token` in
# `${XDG_CONFIG_HOME:-$HOME/.config}/opencode/envoy.json`, the same file and keys the production
# daemon launcher reads (~/.config/legion/<team>/run-daemon.sh). There is no third source --
# DISPATCH_TOKEN is not a secretsd key on this machine (LEGION-40) -- and nothing is guessed: when
# neither source has a value the caller stops, naming the variable, the file, and the key, before
# anything starts. A bare `jq -r` prints the literal `null` for a missing key and would hand curl a
# bearer of "null"; here `?` swallows the type error of a missing/non-object `.dispatch` (jaq, which
# is the `jq` on this box's PATH, raises one where jq 1.7 does not), `// empty` swallows the null,
# and the `-n` check refuses the blank. The value is assigned, not exported: it reaches a child only
# where the caller passes it on purpose (up.sh's daemon `env` block, the curl Authorization header).
resolve_dispatch_config() {
  local envoy_json="${XDG_CONFIG_HOME:-$HOME/.config}/opencode/envoy.json"
  local variable key value

  for variable in DISPATCH_URL DISPATCH_TOKEN; do
    [[ -z "${!variable:-}" ]] || continue
    case "$variable" in
      DISPATCH_URL) key='.dispatch.serverUrl' ;;
      DISPATCH_TOKEN) key='.dispatch.token' ;;
    esac
    [[ -f "$envoy_json" ]] ||
      fail "${variable} is unset and ${envoy_json} does not exist; export ${variable} or set ${key} in that file"
    value="$(jq -r "${key}? // empty" "$envoy_json")" ||
      fail "${variable} is unset and ${envoy_json} could not be parsed as JSON (see the jq error above); export ${variable} or set ${key} in that file"
    [[ -n "$value" ]] ||
      fail "${variable} is unset and ${envoy_json} has no ${key}; export ${variable} or set ${key} in that file"
    printf -v "$variable" '%s' "$value"
  done
}
