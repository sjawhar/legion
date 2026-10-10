#!/usr/bin/env bash
# sessions-import-pods.test.sh — Proves which volume each import pod of sessions-import-pods.sh
# mounts, how it is scheduled, and what stops the script before any pod, over a stand-in kubectl on
# PATH. The stand-in holds Sandboxes, volumes and pods as JSON lists, labelled as legion-v10.0.0
# labels them, answers equality and `!key` label selectors over them, records every call, keeps
# each Pod it is asked to create, and reports each import pod with the phase $state/phase names.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly project_root
readonly script="${project_root}/scripts/sessions-import-pods.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
readonly bin="${temporary_dir}/bin" state="${temporary_dir}/state"
mkdir -p "$bin" "$state/created"

cat >"${bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state=$KUBECTL_STATE
echo "$*" >>"$state/calls"
args=() selector='' field='' output=''
while (($#)); do
  case $1 in
  -n | --context) shift 2 ;;
  -l) selector=$2; shift 2 ;;
  --field-selector=*) field=${1#--field-selector=}; shift ;;
  -o) output=$2; shift 2 ;;
  *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]}"
# matching lists the items of $1's list that every term of the label selector and of the field
# selector (status.phase=X and status.phase!=X terms) admits.
matching() {
  jq --arg s "$selector" --arg f "$field" '[.items[] | (.metadata.labels // {}) as $l | (.status.phase // "") as $phase | select(
    ($s | split(",") | all(. as $t |
      if ($t | startswith("!")) then ($l | has($t[1:]) | not)
      elif ($t | contains("=")) then ($l[$t | split("=")[0]] == ($t | split("=")[1]))
      else ($l | has($t)) end)) and
    ($f | split(",") | map(select(. != "")) | all(. as $t |
      if ($t | contains("!=")) then $phase != ($t | split("!=")[1]) else $phase == ($t | split("=")[1]) end)))]' "$state/$1.json"
}
case "$1 $2" in
"delete configmap" | "create configmap" | "delete pod") ;;
"logs "*) echo "import lines of $2" ;;
"create -f")
  manifest=$(cat)
  name=$(sed -n 's/^  name: //p' <<<"$manifest" | head -1)
  printf '%s\n' "$manifest" >"$state/created/$name.yaml"
  ;;
"get sandboxes" | "get pvc" | "get pods")
  items=$(matching "$2")
  case $output in
  name) jq -r --arg k "$2" '.[] | "\($k)/\(.metadata.name)"' <<<"$items" ;;
  json) jq '{items: .}' <<<"$items" ;;
  jsonpath=*) jq -r '[.[].metadata.name] | join(" ")' <<<"$items" ;;
  esac
  ;;
"get pod") cat "$state/phase" ;;
*) echo "stand-in kubectl: unexpected call: $*" >&2; exit 64 ;;
esac
EOF
chmod +x "${bin}/kubectl"

readonly project=acme-widgets
readonly image=ghcr.io/sjawhar/legion-worker@sha256:0000000000000000000000000000000000000000000000000000000000000000
readonly pool='{"key":"legion.dev/pool","operator":"Equal","value":"legion","effect":"NoSchedule"}'
readonly dedicated='{"key":"example.internal/dedicated","operator":"Exists","effect":"NoSchedule"}'

cat >"$state/claims.json" <<'EOF'
{"claims": [
  {"token": "legion-acmewidgets-infra-1234-architect", "tree": "INFRA-1234", "issue": "INFRA-1234", "role": "architect", "sessionFile": "/s/a.jsonl"},
  {"token": "legion-acmewidgets-infra-1235-planner", "tree": "INFRA-1234", "issue": "INFRA-1235", "role": "planner", "sessionFile": "/s/b.jsonl"},
  {"token": "legion-acmewidgets-acme-7-architect", "tree": "ACME-7", "issue": "ACME-7", "role": "architect", "sessionFile": "/s/c.jsonl"},
  {"token": "legion-acmewidgets-controller", "tree": "", "issue": "", "role": "controller", "sessionFile": "/s/d.jsonl"}
]}
EOF
# v10's objects: each Sandbox and the root's volume carry project, role, tree and issue labels, the
# controller's project and role alone. The root Sandbox's name is v10's hashed SandboxName, which
# the script never computes.
labels() { jq -cn --arg r "$1" --arg t "$2" --arg i "$3" '{"legion.dev/project": "acmewidgets", "legion.dev/role": $r} + (if $t == "" then {} else {"legion.dev/tree": $t, "legion.dev/issue": $i} end)'; }
v10_state() {
  jq -n --argjson a "$(labels architect INFRA-1234 INFRA-1234)" --argjson p "$(labels planner INFRA-1234 INFRA-1235)" \
    --argjson b "$(labels architect ACME-7 ACME-7)" --argjson c "$(labels controller '' '')" \
    --argjson pool "$pool" --argjson dedicated "$dedicated" '{items: [
    {metadata: {name: "legion-acmewidgets-infra-1234-archite-59acf6b9", labels: $a}, spec: {podTemplate: {spec: {
      nodeSelector: {"legion.dev/pool": "legion", "example.internal/zone": "a"}, tolerations: [$pool, $dedicated],
      priorityClassName: "legion-high", serviceAccountName: "legion-worker"}}}},
    {metadata: {name: "legion-acmewidgets-infra-1235-planner", labels: $p}, spec: {podTemplate: {spec: {}}}},
    {metadata: {name: "legion-acmewidgets-acme-7-architect", labels: $b}, spec: {podTemplate: {spec: {
      nodeSelector: {"legion.dev/pool": "legion"}, tolerations: [$pool]}}}},
    {metadata: {name: "legion-acmewidgets-controller", labels: $c}, spec: {podTemplate: {spec: {}}}}
  ] | map(.spec.operatingMode = "Suspended")}' >"$state/sandboxes.json"
  jq -n --argjson a "$(labels architect INFRA-1234 INFRA-1234)" --argjson b "$(labels architect ACME-7 ACME-7)" '{items: [
    {metadata: {name: "tree-legion-acmewidgets-infra-1234-archite-59acf6b9", labels: $a}},
    {metadata: {name: "tree-legion-acmewidgets-acme-7-architect", labels: $b}}
  ]}' >"$state/pvc.json"
  jq -n --argjson a "$(labels architect INFRA-1234 INFRA-1234)" '{items: [
    {metadata: {name: "legion-acmewidgets-infra-1234-archite-59acf6b9", labels: $a}, status: {phase: "Succeeded"}}]}' >"$state/pods.json"
  echo Succeeded >"$state/phase"
}

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
run() {
  rm -f "$state/calls" "$state"/created/*.yaml
  set +e
  PATH="$bin:$PATH" KUBECTL_STATE="$state" bash "$script" "$state/claims.json" legion "$image" "$project" legion-acme-providers SESSION_DSN --context test \
    >"$temporary_dir/out" 2>"$temporary_dir/err"
  code=$?
  set -e
}
field() { sed -n "s/^  $2: //p" "$state/created/$1.yaml"; }
expect_pod() {
  local pod=$1 volume=$2 tree=$3 node_selector=$4 tolerations=$5 priority=$6
  local manifest="$state/created/$pod.yaml"
  [[ -f $manifest ]] || { fail "no pod $pod was created"; return; }
  grep -qF "claimName: \"$volume\"" "$manifest" || fail "$pod mounts $(grep -o 'claimName: "[^"]*"' "$manifest"), want $volume"
  grep -qF -- "--tree, \"$tree\", --tree-volume, /legion]" "$manifest" || fail "$pod does not import $tree: $(grep 'args:' "$manifest")"
  [[ $(field "$pod" nodeSelector | jq -cS .) == "$(jq -cS . <<<"$node_selector")" ]] || fail "$pod nodeSelector $(field "$pod" nodeSelector), want $node_selector"
  [[ $(field "$pod" tolerations | jq -cS .) == "$(jq -cS . <<<"$tolerations")" ]] || fail "$pod tolerations $(field "$pod" tolerations), want $tolerations"
  [[ $(field "$pod" priorityClassName | jq -r .) == "$priority" ]] || fail "$pod priorityClassName $(field "$pod" priorityClassName), want \"$priority\""
  # It runs as the namespace's default ServiceAccount with no token, whatever the root Sandbox's
  # pods ran as: a namespace admission policy may let only the agent-sandbox controller create pods
  # as the worker ServiceAccount.
  [[ -z $(field "$pod" serviceAccountName) ]] || fail "$pod serviceAccountName $(field "$pod" serviceAccountName), want none"
  [[ $(field "$pod" automountServiceAccountToken) == false ]] || fail "$pod automountServiceAccountToken $(field "$pod" automountServiceAccountToken), want false"
}

# Every tree: each pod mounts the volume its root's labels find, whatever the name, scheduled as
# that root Sandbox's pods are; a leftover pod and ConfigMap are deleted before each is created;
# the claim of no tree is named for step 5.
v10_state
run
((code == 0)) || fail "every tree: exit $code, want 0; stderr: $(<"$temporary_dir/err")"
expect_pod legion-sessions-import-infra-1234 tree-legion-acmewidgets-infra-1234-archite-59acf6b9 INFRA-1234 \
  '{"legion.dev/pool":"legion","example.internal/zone":"a"}' "[$pool,$dedicated]" legion-high
expect_pod legion-sessions-import-acme-7 tree-legion-acmewidgets-acme-7-architect ACME-7 '{"legion.dev/pool":"legion"}' "[$pool]" ""
grep -qF "delete pod legion-sessions-import-acme-7 --ignore-not-found --wait" "$state/calls" || fail "a leftover import pod was not deleted first"
grep -qF "delete configmap legion-sessions-import-claims --ignore-not-found" "$state/calls" || fail "a leftover ConfigMap was not deleted first"
grep -qF "create configmap legion-sessions-import-claims" "$state/calls" || fail "the claims were not created as a ConfigMap"
grep -q "apply" "$state/calls" && fail "the script ran a client-side apply: $(grep apply "$state/calls")"
grep -qF "legion-acmewidgets-controller records a session and belongs to no tree" "$temporary_dir/err" || fail "the claim of no tree was not named: $(<"$temporary_dir/err")"
grep -qF -- "--context test" "$state/calls" || fail "the kubectl arguments did not reach kubectl"

# A failed import pod is reported at once, not after the 15-minute wait.
echo Failed >"$state/phase"
started=$SECONDS
run
((code == 1)) || fail "a failed pod: exit $code, want 1"
((SECONDS - started < 30)) || fail "a failed pod took $((SECONDS - started)) s to report"
grep -qF "import pod ended Failed" "$temporary_dir/err" || fail "the failed pod was not named: $(<"$temporary_dir/err")"

# A tree whose volume is gone fails, and the others still run.
v10_state
jq '.items |= map(select(.metadata.labels["legion.dev/tree"] != "ACME-7"))' "$state/pvc.json" >"$state/pvc.new" && mv "$state/pvc.new" "$state/pvc.json"
run
((code == 1)) || fail "a lost volume: exit $code, want 1"
grep -qF "tree ACME-7 has no volume labelled" "$temporary_dir/err" || fail "the lost volume was not named: $(<"$temporary_dir/err")"
[[ -f $state/created/legion-sessions-import-infra-1234.yaml ]] || fail "the other trees did not run beside the failed one"

# A pod of the project that has not ended, or a Sandbox not Suspended, whose pod may yet start,
# stops the script before anything is created.
v10_state
jq '.items[0].status.phase = "Pending"' "$state/pods.json" >"$state/pods.new" && mv "$state/pods.new" "$state/pods.json"
run
((code == 2)) || fail "a pending pod: exit $code, want 2"
grep -qF "pod(s) of project acmewidgets have not ended" "$temporary_dir/err" || fail "the pending pod was not named: $(<"$temporary_dir/err")"
compgen -G "$state/created/*.yaml" >/dev/null && fail "a pod was created while a pod of the project had not ended"
v10_state
jq '.items[2].spec.operatingMode = "Running" | .items[3].spec |= del(.operatingMode)' "$state/sandboxes.json" >"$state/sandboxes.new" && mv "$state/sandboxes.new" "$state/sandboxes.json"
run
((code == 2)) || fail "an awake Sandbox: exit $code, want 2"
grep -qF "2 Sandbox(es) of project acmewidgets are not Suspended, e.g. sandbox/legion-acmewidgets-acme-7-architect" "$temporary_dir/err" ||
  fail "the awake Sandboxes, one Running and one with no mode, were not named: $(<"$temporary_dir/err")"
compgen -G "$state/created/*.yaml" >/dev/null && fail "a pod was created while a Sandbox of the project was not Suspended"

# A project on a release with issue pods is refused by name before anything is created.
v10_state
jq '.items += [{metadata: {name: "legion-acmewidgets-acme-8", labels: {"legion.dev/project": "acmewidgets", "legion.dev/tree": "ACME-8", "legion.dev/issue": "ACME-8"}}}]' \
  "$state/sandboxes.json" >"$state/sandboxes.new" && mv "$state/sandboxes.new" "$state/sandboxes.json"
run
((code == 2)) || fail "an issue-pod release: exit $code, want 2"
grep -qF "legion-v10.1.0 or later; this script copies only from legion-v10.0.0" "$temporary_dir/err" || fail "the release was not refused by name: $(<"$temporary_dir/err")"
compgen -G "$state/created/*.yaml" >/dev/null && fail "a pod was created for an issue-pod release"

if ((failures > 0)); then
  printf '%d failure(s)\n' "$failures" >&2
  exit 1
fi
echo "sessions-import-pods.test.sh: ok"
