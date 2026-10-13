# shellcheck shell=bash disable=SC2034 # the operator_dispatch_* variables are the caller's to read
# The operator's Dispatch, for a script run from the operator's shell. Sourced, never run.
#
# operator_dispatch resolves it as every command does, through lib/operator-dispatch.ts: the
# envoy.json `envoyConfigPath("command")` chooses (ENVOY_CONFIG_FILE, which the operator's own
# session sets, then the omp profile, Claude Code and OpenCode directories, then
# ${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json), the working directory's .envoy/envoy.json, and
# the environment as it is when the function is called, read by `dispatchSettings`: its URL and
# bearer, the same-server rule for a file's token, and a `!` token run. It sets
# operator_dispatch_file to the envoy.json that rule chose, and operator_dispatch_url and
# operator_dispatch_token to Dispatch's URL and bearer; when nothing configures Dispatch or a file
# turns it off, both are empty and operator_dispatch_notice says why. A file, a URL or a token that
# prevents Dispatch fails the call through the caller's `fail`, naming it. The bearer stays in this
# shell's unexported variable: it reaches no argv and no process's environment.
operator_dispatch_lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)

operator_dispatch() {
  local resolved reason status=0
  operator_dispatch_file='' operator_dispatch_url='' operator_dispatch_token='' operator_dispatch_notice=''
  resolved=$(bun "$operator_dispatch_lib/operator-dispatch.ts") || status=$?
  if [ "$status" -ne 0 ]; then
    reason=$(jq -r '.error // empty' <<<"$resolved" 2>/dev/null) || reason=''
    fail "the operator's Dispatch: ${reason:-lib/operator-dispatch.ts exited $status}"
  fi
  operator_dispatch_file=$(jq -r '.file' <<<"$resolved")
  operator_dispatch_url=$(jq -r '.url // empty' <<<"$resolved")
  operator_dispatch_token=$(jq -r '.token // empty' <<<"$resolved")
  operator_dispatch_notice=$(jq -r '.notice // empty' <<<"$resolved")
}
