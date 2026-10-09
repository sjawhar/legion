#!/usr/bin/env bash
# sessions-import-pods.test.sh — Proves which volume each import pod of sessions-import-pods.sh
# mounts and how it is scheduled, over a stand-in kubectl on PATH that holds a few volumes and
# Sandboxes, records every call, keeps each Pod it is asked to apply, and reports each pod
# Succeeded. The volume names are the earlier release's own (SandboxName and TreeClaimName in
# internal/runtime/sandbox/names.go at legion-v10.0.0, run on these tokens): a root Sandbox name
# past 63 characters is hashed, and one of 59 is not, though its volume's name is 64.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly project_root
readonly script="${project_root}/scripts/sessions-import-pods.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
readonly bin="${temporary_dir}/bin" state="${temporary_dir}/state"
mkdir -p "$bin" "$state/sandboxes" "$state/applied"

# The stand-in answers the calls the script makes. Every call is one line of $state/calls; `get pvc`
# finds a volume named in $state/pvcs, `get sandboxes` prints $state/sandboxes/<name>.json, and an
# applied Pod is kept as $state/applied/<name>.yaml.
cat >"${bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state=$KUBECTL_STATE
echo "$*" >>"$state/calls"
args=()
while (($#)); do
  case $1 in
  -n | --context) shift 2 ;;
  *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]}"
case "$1 $2" in
"create configmap") echo "kind: ConfigMap" ;;
"apply -f")
  manifest=$(cat)
  if grep -q '^kind: Pod$' <<<"$manifest"; then
    name=$(sed -n 's/^  name: //p' <<<"$manifest" | head -1)
    printf '%s\n' "$manifest" >"$state/applied/$name.yaml"
  fi
  ;;
"get pvc") grep -qFx "$3" "$state/pvcs" || { echo "Error from server (NotFound): persistentvolumeclaims \"$3\" not found" >&2; exit 1; } ;;
"get sandboxes")
  [[ -f $state/sandboxes/$3.json ]] || { echo "Error from server (NotFound): sandboxes \"$3\" not found" >&2; exit 1; }
  cat "$state/sandboxes/$3.json"
  ;;
"get pod") echo Succeeded ;;
"logs "*) echo "import lines of $2" ;;
"wait "* | "delete "*) ;;
*) echo "stand-in kubectl: unexpected call: $*" >&2; exit 64 ;;
esac
EOF
chmod +x "${bin}/kubectl"

readonly project=acme-widgets-platform-deployment
readonly long_tree=INFRASTRUCTURE-1234 short_tree=INFRA-123456 lost_tree=ACME-7
# legion-v10.0.0's SandboxName of legion-acmewidgetsplatformdeployment-infrastructure-1234-architect
# (66 characters), and of legion-acmewidgetsplatformdeployment-infra-123456-architect (59).
readonly long_root=legion-acmewidgetsplatformdeployment-infrastructure-12-59acf6b9
readonly short_root=legion-acmewidgetsplatformdeployment-infra-123456-architect
readonly controller=legion-acmewidgetsplatformdeployment-controller

cat >"$state/claims.json" <<EOF
{"claims": [
  {"token": "legion-acmewidgetsplatformdeployment-infrastructure-1234-architect", "tree": "$long_tree", "issue": "$long_tree", "role": "architect", "sessionFile": "/s/a.jsonl"},
  {"token": "legion-acmewidgetsplatformdeployment-infrastructure-1235-planner", "tree": "$long_tree", "issue": "INFRASTRUCTURE-1235", "role": "planner", "sessionFile": "/s/b.jsonl"},
  {"token": "$short_root", "tree": "$short_tree", "issue": "$short_tree", "role": "architect", "sessionFile": "/s/c.jsonl"},
  {"token": "$controller", "tree": "", "issue": "", "role": "controller", "sessionFile": "/s/d.jsonl"}
]}
EOF
printf '%s\n' "tree-$long_root" "tree-$short_root" "tree-$controller" >"$state/pvcs"
cat >"$state/sandboxes/$long_root.json" <<'EOF'
{"spec": {"podTemplate": {"spec": {
  "nodeSelector": {"legion.dev/pool": "legion", "example.internal/zone": "a"},
  "tolerations": [{"key": "legion.dev/pool", "operator": "Equal", "value": "legion", "effect": "NoSchedule"},
                  {"key": "example.internal/dedicated", "operator": "Exists", "effect": "NoSchedule"}],
  "priorityClassName": "legion-high", "serviceAccountName": "legion-worker", "containers": []}}}}
EOF
cat >"$state/sandboxes/$short_root.json" <<'EOF'
{"spec": {"podTemplate": {"spec": {"nodeSelector": {"legion.dev/pool": "legion"},
  "tolerations": [{"key": "legion.dev/pool", "operator": "Equal", "value": "legion", "effect": "NoSchedule"}], "containers": []}}}}
EOF
cp "$state/sandboxes/$long_root.json" "$state/sandboxes/$controller.json"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

# run runs the script against the stand-in, keeping its exit, stdout and stderr.
run() {
  rm -f "$state/calls" "$state"/applied/*.yaml
  set +e
  PATH="$bin:$PATH" KUBECTL_STATE="$state" bash "$script" "$@" >"$temporary_dir/out" 2>"$temporary_dir/err"
  code=$?
  set -e
}

# field is the JSON a manifest line of the pod spec carries, e.g. `field <pod> nodeSelector`.
field() {
  sed -n "s/^  $2: //p" "$state/applied/$1.yaml"
}

expect_pod() {
  local pod=$1 volume=$2 selection=$3 node_selector=$4 tolerations=$5 priority=$6 account=$7
  local manifest="$state/applied/$pod.yaml"
  [[ -f $manifest ]] || { fail "no pod $pod was applied"; return; }
  grep -qF "claimName: \"$volume\"" "$manifest" || fail "$pod mounts $(grep -o 'claimName: "[^"]*"' "$manifest"), want $volume"
  grep -qF "\"$selection\", --tree-volume, /legion]" "$manifest" || fail "$pod does not import $selection: $(grep 'args:' "$manifest")"
  [[ $(field "$pod" nodeSelector | jq -cS .) == "$(jq -cS . <<<"$node_selector")" ]] || fail "$pod nodeSelector $(field "$pod" nodeSelector), want $node_selector"
  [[ $(field "$pod" tolerations | jq -cS .) == "$(jq -cS . <<<"$tolerations")" ]] || fail "$pod tolerations $(field "$pod" tolerations), want $tolerations"
  [[ $(field "$pod" priorityClassName | jq -r .) == "$priority" ]] || fail "$pod priorityClassName $(field "$pod" priorityClassName), want \"$priority\""
  [[ $(field "$pod" serviceAccountName | jq -r .) == "$account" ]] || fail "$pod serviceAccountName $(field "$pod" serviceAccountName), want \"$account\""
}

readonly pool='{"key":"legion.dev/pool","operator":"Equal","value":"legion","effect":"NoSchedule"}'
readonly dedicated='{"key":"example.internal/dedicated","operator":"Exists","effect":"NoSchedule"}'
readonly image=ghcr.io/sjawhar/legion-worker@sha256:0000000000000000000000000000000000000000000000000000000000000000

# Every tree: each pod mounts its root Sandbox's volume, hashed name and all, and is scheduled as
# that Sandbox's pods are; the claim of no tree is named for --claim, and nothing fails.
run "$state/claims.json" legion "$image" "$project" legion-acme-providers SESSION_DSN --context test
((code == 0)) || fail "every tree: exit $code, want 0; stderr: $(<"$temporary_dir/err")"
expect_pod "legion-sessions-import-$(tr '[:upper:]' '[:lower:]' <<<"$long_tree")" "tree-$long_root" "$long_tree" \
  '{"legion.dev/pool":"legion","example.internal/zone":"a"}' "[$pool,$dedicated]" legion-high legion-worker
expect_pod "legion-sessions-import-$(tr '[:upper:]' '[:lower:]' <<<"$short_tree")" "tree-$short_root" "$short_tree" \
  '{"legion.dev/pool":"legion"}' "[$pool]" "" ""
grep -qF "get sandboxes $long_root -o json" "$state/calls" || fail "the long tree's Sandbox $long_root was never read"
grep -qF "$controller records a session and belongs to no tree" "$temporary_dir/err" || fail "the controller's claim was not named: $(<"$temporary_dir/err")"
grep -qF -- "--context test" "$state/calls" || fail "the kubectl arguments did not reach kubectl"

# One claim on a named volume: scheduled as that claim's own Sandbox.
run --claim "$controller" --pvc "tree-$controller" "$state/claims.json" legion "$image" "$project" legion-acme-providers SESSION_DSN
((code == 0)) || fail "--claim: exit $code, want 0; stderr: $(<"$temporary_dir/err")"
expect_pod legion-sessions-import-claim "tree-$controller" "$controller" \
  '{"legion.dev/pool":"legion","example.internal/zone":"a"}' "[$pool,$dedicated]" legion-high legion-worker

# A tree whose volume is gone, and one whose root Sandbox is gone, each fail with nothing applied
# for it; the other trees still run.
jq --arg t "$lost_tree" '.claims += [{"token": "legion-acmewidgetsplatformdeployment-acme-7-architect", "tree": $t, "issue": $t, "role": "architect", "sessionFile": "/s/e.jsonl"}]' \
  "$state/claims.json" >"$state/lost.json"
run "$state/lost.json" legion "$image" "$project" legion-acme-providers SESSION_DSN
((code == 1)) || fail "a lost volume: exit $code, want 1"
grep -qF "$lost_tree has no volume tree-legion-acmewidgetsplatformdeployment-acme-7-architect" "$temporary_dir/err" || fail "the lost volume was not named: $(<"$temporary_dir/err")"
echo "tree-legion-acmewidgetsplatformdeployment-acme-7-architect" >>"$state/pvcs"
run "$state/lost.json" legion "$image" "$project" legion-acme-providers SESSION_DSN
((code == 1)) || fail "a lost Sandbox: exit $code, want 1"
grep -qF "$lost_tree has no Sandbox legion-acmewidgetsplatformdeployment-acme-7-architect" "$temporary_dir/err" || fail "the lost Sandbox was not named: $(<"$temporary_dir/err")"
[[ ! -f $state/applied/legion-sessions-import-acme-7.yaml ]] || fail "a pod was applied for the tree whose Sandbox is gone"
[[ -f $state/applied/legion-sessions-import-infra-123456.yaml ]] || fail "the other trees did not run beside the failed one"

if ((failures > 0)); then
  printf '%d failure(s)\n' "$failures" >&2
  exit 1
fi
echo "sessions-import-pods.test.sh: ok"
