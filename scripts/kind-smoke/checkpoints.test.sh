#!/usr/bin/env bash
# Harness for scripts/kind-smoke/checkpoints.sh: curl and kubectl are PATH fakes serving fixtures
# under $FIX (a NAME.json file, or NAME.seq — one fixture file name per line, served in order and
# the last one repeated), so every verdict is pinned without a cluster, a daemon, or a network.
set -euo pipefail
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
fake_bin="$tmp/bin"
mkdir -p "$fake_bin"
export FAKE_LOG="$tmp/calls.log"
: >"$FAKE_LOG"
export FIX="$tmp/fix"
mkdir -p "$FIX"
fake() {
  {
    printf '#!/usr/bin/env bash\nprintf "%%s %%s\\n" %q "$*" >>"$FAKE_LOG"\n' "$1"
    cat
  } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}
export PATH="$fake_bin:$PATH"
# serve NAME — $FIX/NAME.seq (one fixture per call, last repeated) else $FIX/NAME.json else {}
cat >"$fake_bin/serve" <<'EOF'
#!/usr/bin/env bash
name="$1"
if [ -f "$FIX/$name.seq" ]; then
  n="$(cat "$FIX/$name.counter" 2>/dev/null || echo 0)"; n=$((n + 1)); echo "$n" >"$FIX/$name.counter"
  total="$(wc -l <"$FIX/$name.seq")"; [ "$n" -gt "$total" ] && n="$total"
  cat "$FIX/$(sed -n "${n}p" "$FIX/$name.seq")"
elif [ -f "$FIX/$name.json" ]; then cat "$FIX/$name.json"
else echo '{}'; fi
EOF
chmod +x "$fake_bin/serve"
fake curl <<'EOF'
url=""; for a in "$@"; do case "$a" in http*) url="$a" ;; esac; done
path="${url#*://*/}"
case "$path" in
  legion/v1/state) serve state ;;
  api/v1/issues/*/asks*) k="${path#api/v1/issues/}"; k="${k%%/*}"; [ -f "$FIX/asks-$k.json" ] && cat "$FIX/asks-$k.json" || echo '[]' ;;
  api/v1/issues/*) k="${path#api/v1/issues/}"; serve "issue-$k" ;;
  api/v1/issues\?*parent=*) k="${path##*parent=}"; k="${k%%&*}"; [ -f "$FIX/children-$k.json" ] && cat "$FIX/children-$k.json" || echo '[]' ;;
  *) echo "unexpected curl request: $*" >&2; exit 1 ;;
esac
EOF
fake kubectl <<'EOF'
all="$*"
case "$all" in
  *" get pod "*" -o json") n="${all#* get pod }"; n="${n%% *}"; serve "pod-$n" ;;
  *" get pvc "*" -o json") n="${all#* get pvc }"; n="${n%% *}"; serve "pvc-$n" ;;
  *" get pods "*"-o json") serve pods ;;
  *" get deploy "*"-o json") serve deploy ;;
  *" delete pod "*) n="${all#* delete pod }"; n="${n%% *}"; echo "deleted-$n" >>"$FIX/deleted" ;;
  *" exec "*" cat /proc/1/environ") n="${all#* exec }"; n="${n%% *}"; cat "$FIX/environ-$n" 2>/dev/null ;;
  *" exec "*"ls "*) n="${all#* exec }"; n="${n%% *}"; cat "$FIX/ls-$n" 2>/dev/null ;;
  *" logs "*) n="${all#* logs }"; n="${n%% *}"; cat "$FIX/logs-$n" 2>/dev/null || echo "(no log)" ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF

state_dir="$tmp/state"
plant_records() { # plant_records [controller line] — the records up.sh writes
  rm -rf "$state_dir"
  mkdir -p "$state_dir/records" "$state_dir/logs" "$state_dir/pids" "$state_dir/overlay/secrets"
  mkdir -p -m 0700 "$state_dir/secrets"
  local r
  for r in instance=t1 port-base=41000 gateway=172.30.0.1 project=demo dispatch-project=ST1 github-ingress=none session-store=pvc worker-cap=6 root-issue-count=1 repo=sjawhar/legion-smoke; do
    echo "${r#*=}" >"$state_dir/records/${r%%=*}"
  done
  echo ST1-1 >"$state_dir/records/root-issues"
  echo "${1:-none: the checkout has no legion controller start (pull request #1110)}" >"$state_dir/records/controller"
  echo "$state_dir/kubeconfig" >"$state_dir/records/kubeconfig"
  printf 'apiVersion: v1\nkind: Config\n' >"$state_dir/kubeconfig"
  jq -n '{role_profiles:{architect:"small",planner:"small",implementer:"medium",tester:"large",reviewer:"small",merger:"small"},
          resources:{small:{requests:{cpu:"500m",memory:"1Gi","ephemeral-storage":"2Gi"},limits:{cpu:"2",memory:"3Gi","ephemeral-storage":"8Gi"}},
                     medium:{requests:{cpu:"1",memory:"2Gi","ephemeral-storage":"10Gi"},limits:{cpu:"4",memory:"6Gi","ephemeral-storage":"30Gi"}},
                     large:{requests:{cpu:"2",memory:"4Gi","ephemeral-storage":"20Gi"},limits:{cpu:"6",memory:"12Gi","ephemeral-storage":"60Gi"}}}}' >"$state_dir/records/profiles.json"
  echo dispatch-secret-value-0123456789 >"$state_dir/secrets/dispatch-token"
  echo envoy-secret-value-0123456789 >"$state_dir/secrets/envoy-token"
  echo pg-secret-value-0123456789 >"$state_dir/secrets/postgres-password"
  printf 'DISPATCH_TOKEN=dispatch-secret-value-0123456789\nENVOY_TOKEN=envoy-secret-value-0123456789\nANTHROPIC_API_KEY=anthropic-canary-value\n' >"$state_dir/overlay/secrets/providers.env"
}
reset_fixtures() { rm -rf "$FIX"; mkdir -p "$FIX"; }
run_cp() { # run_cp NAME ENV… → $tmp/out.txt, exit code returned
  local name="$1"
  shift
  set +e
  env SMOKE_DIR="$state_dir" SMOKE_INSTANCE=t1 SMOKE_POLL_INTERVAL=0 \
    SMOKE_WAIT_ADMITTED=1 SMOKE_WAIT_ARCHITECT_POD=1 SMOKE_WAIT_SPEC_POSTED=1 SMOKE_WAIT_TREE_MOVED=1 \
    SMOKE_WAIT_KILL_PHASE=3 SMOKE_WAIT_KILL_RESUME=3 SMOKE_WAIT_KILL_COMPLETE=3 SMOKE_WAIT_CAP_QUEUE=3 SMOKE_WAIT_CAP_PROMOTE=3 SMOKE_WAIT_DONE=1 \
    "$@" bash "$here/checkpoints.sh" "$name" >"$tmp/out.txt" 2>&1
  local status=$?
  set -e
  return $status
}
expect_verdict() { # expect_verdict OK|FAILED|SKIPPED-BLOCKED EXIT NAME 'substring' ENV…
  local verdict="$1" code="$2" name="$3" want="$4"
  shift 4
  local status=0
  run_cp "$name" "$@" || status=$?
  [ "$status" = "$code" ] || { echo "$name: expected exit $code, got $status" >&2; cat "$tmp/out.txt" >&2; exit 1; }
  [ "$(wc -l <"$tmp/out.txt")" = 1 ] || { echo "$name: expected exactly one line" >&2; cat "$tmp/out.txt" >&2; exit 1; }
  grep -Fq "CHECKPOINT $name $verdict" "$tmp/out.txt" || { echo "$name: missing verdict $verdict" >&2; cat "$tmp/out.txt" >&2; exit 1; }
  grep -Fq -- "$want" "$tmp/out.txt" || { echo "$name: missing text: $want" >&2; cat "$tmp/out.txt" >&2; exit 1; }
}
expect_ok() { expect_verdict OK 0 "$@"; }
expect_failed() { expect_verdict FAILED 1 "$@"; }
expect_blocked() { expect_verdict SKIPPED-BLOCKED 3 "$@"; }

# state fixtures
base_state() { # base_state → a state document with one active tree and its architect claim
  jq -n '{project:"demo",version:33,
    issues:{"ST1-1":{key:"ST1-1",title:"t",status:"in_progress",children:[]}},
    trees:{"ST1-1":{status:"active",generation:1,launchFailures:0,readyConfirmedAt:"2026-09-15T00:00:00Z",
      locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-1-architect-g1",podUid:"u1",pvcName:"legion-st1-1",ompSessionFile:"/home/legion/.omp/profiles/legion/agent/sessions/--x--/2026-09-15T00-00-00-000Z_arch.jsonl"}}},
    admission:{cap:3,active:["ST1-1"],queue:[]},gates:{},
    roles:{"legion-demo-st1-1-architect":{role:"architect",issue:"ST1-1",generation:1,sessionId:"arch",readyConfirmedAt:"2026-09-15T00:00:00Z",launchFailures:0,
      locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-1-architect-g1",podUid:"u1",pvcName:"legion-st1-1"}}},
    controllerPendingNotices:0,pendingStatusWrites:[],workerAdmission:{queue:[]}}'
}
pod_fixture() { # pod_fixture NAME ROLE ISSUE GEN PHASE [PROFILE] [RESUME] → a pod document
  local name="$1" role="$2" issue="$3" gen="$4" phase="$5" profile="${6:-small}" resume="${7:-}"
  jq -n --arg n "$name" --arg r "$role" --arg i "$issue" --arg g "$gen" --arg p "$phase" --arg res "$resume" \
    --argjson prof "$(jq --arg p "$profile" '.resources[$p]' "$state_dir/records/profiles.json")" '
    {metadata:{name:$n,labels:{"legion.dev/project":"demo","legion.dev/tree":"st1-1","legion.dev/issue":($i|ascii_downcase),"legion.dev/role":$r,"legion.dev/generation":$g}},
     spec:{volumes:[{name:"tree",persistentVolumeClaim:{claimName:"legion-st1-1"}},{name:"boot",secret:{secretName:$n}}],
           initContainers:[{name:"workspace-init",command:["legion","workspace-init","--issue",$i],env:[{name:"LEGION_PROVISION_TOKEN_FILE",value:"/var/run/legion/provision/token"}],resources:$prof}],
           containers:[{name:"worker",command:(["/opt/legion/bin/legion","worker-shim","--connect","tcp://legion-daemon-demo.legion.svc:13371","--boot-token-file","/var/run/legion/boot/LEGION_BOOT_TOKEN","--provider-env-dir","/var/run/legion/providers","--","omp"] + (if $res == "" then [] else ["--resume=" + $res] end) + ["--mode","rpc","--append-system-prompt","x"]),
                        env:[{name:"LEGION_TREE",value:"ST1-1"},{name:"LEGION_ISSUE",value:$i},{name:"LEGION_ROLE",value:$r}],resources:$prof}]},
     status:{phase:$p}}'
}
issue_fixture() { jq -n --arg k "$1" --arg s "$2" --arg a "${3:-}" --argjson c "${4:-[]}" '{key:$k,status:$s,primary_artifact_id:(if $a == "" then null else $a end),children:$c}'; }

# ---- admitted ---------------------------------------------------------------------------------
plant_records
reset_fixtures
base_state >"$FIX/state.json"
issue_fixture ST1-1 in_progress >"$FIX/issue-ST1-1.json"
expect_ok admitted 'ST1-1 status=in_progress tree=active controller=none'
base_state | jq '.trees = {}' >"$FIX/state.json"
expect_failed admitted 'no tree recorded for ST1-1'
issue_fixture ST1-1 triage >"$FIX/issue-ST1-1.json"
expect_failed admitted "ST1-1 has Dispatch status 'triage', not a released one"
# with a controller pane recorded, the daemon must show the external controller
plant_records 'tmux legion-smoke-t1 controller'
base_state >"$FIX/state.json"
issue_fixture ST1-1 in_progress >"$FIX/issue-ST1-1.json"
expect_failed admitted 'no external controllerLocator yet'
base_state | jq '.controllerLocator = {runtime:"kubernetes",external:true,sessionId:"c",registeredAt:"2026-09-15T00:00:00Z"}' >"$FIX/state.json"
expect_ok admitted 'controller=external'
# a missing record names up.sh; an unknown checkpoint is usage
rm "$state_dir/records/root-issues"
status=0; run_cp admitted || status=$?
[ "$status" = 1 ] && grep -Fq "error: record root-issues missing under $state_dir: run scripts/kind-smoke/up.sh first" "$tmp/out.txt"
status=0; run_cp not-a-checkpoint || status=$?
[ "$status" = 2 ] && grep -Fq 'usage: checkpoints.sh <admitted|architect-pod|' "$tmp/out.txt"
echo "checkpoints.test.sh: admitted OK"

# ---- architect-pod ------------------------------------------------------------------------------
plant_records
reset_fixtures
base_state >"$FIX/state.json"
issue_fixture ST1-1 in_progress >"$FIX/issue-ST1-1.json"
pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running >"$FIX/pod-legion-st1-1-architect-g1.json"
echo '{"status":{"phase":"Bound"}}' >"$FIX/pvc-legion-st1-1.json"
expect_ok architect-pod 'pod legion-st1-1-architect-g1 Running with its legion.dev labels; claim legion-st1-1 Bound and mounted'
pod_fixture legion-st1-1-architect-g1 architect ST1-1 2 Running >"$FIX/pod-legion-st1-1-architect-g1.json"
expect_failed architect-pod 'label legion.dev/generation is 2, state says 1'
pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Pending >"$FIX/pod-legion-st1-1-architect-g1.json"
expect_failed architect-pod "pod legion-st1-1-architect-g1 is 'Pending', expected Running"
pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running >"$FIX/pod-legion-st1-1-architect-g1.json"
echo '{"status":{"phase":"Pending"}}' >"$FIX/pvc-legion-st1-1.json"
expect_failed architect-pod "claim legion-st1-1 is 'Pending', expected Bound"
echo "checkpoints.test.sh: architect-pod OK"

# ---- spec-posted -----------------------------------------------------------------------------------
plant_records
reset_fixtures
base_state >"$FIX/state.json"
issue_fixture ST1-1 in_progress art-1 '[{"key":"ST1-2"}]' >"$FIX/issue-ST1-1.json"
echo '[{"key":"ST1-2","status":"todo"}]' >"$FIX/children-ST1-1.json"
expect_ok spec-posted 'spec art-1 posted; gate off, none registered; children: ST1-2'
base_state | jq '.gates["ST1-1"] = {artifactId:"art-1",latestVersion:1}' >"$FIX/state.json"
expect_failed spec-posted 'ST1-1 registered a design gate although the run has gates.design: off'
base_state >"$FIX/state.json"
echo '[{"id":"a1","kind":"approval","state":"open"}]' >"$FIX/asks-ST1-1.json"
expect_failed spec-posted 'an approval request is open on ST1-1 although gates.design is off'
rm "$FIX/asks-ST1-1.json"
# a single-issue root: no children, a planner claimed on the root
issue_fixture ST1-1 in_progress art-1 >"$FIX/issue-ST1-1.json"
rm "$FIX/children-ST1-1.json"
base_state | jq '.roles["legion-demo-st1-1-planner"] = {role:"planner",issue:"ST1-1",generation:1,sessionId:"p",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-1-planner-g1",podUid:"u2",pvcName:"legion-st1-1"}}' >"$FIX/state.json"
expect_ok spec-posted 'phase worker: planner'
base_state >"$FIX/state.json"
expect_failed spec-posted 'the tree has not moved past it'
issue_fixture ST1-1 in_progress >"$FIX/issue-ST1-1.json"
expect_failed spec-posted 'ST1-1 has no primary spec document yet'
echo "checkpoints.test.sh: spec-posted OK"

# ---- tree-moved --------------------------------------------------------------------------------------
plant_records
reset_fixtures
issue_fixture ST1-1 in_progress art-1 '[{"key":"ST1-2"}]' >"$FIX/issue-ST1-1.json"
echo '[{"key":"ST1-2","status":"in_progress"}]' >"$FIX/children-ST1-1.json"
base_state | jq '.roles["legion-demo-st1-2-architect"] = {role:"architect",issue:"ST1-2",generation:1,sessionId:"s",readyConfirmedAt:"2026-09-15T00:00:00Z",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-2-architect-g1",podUid:"u3",pvcName:"legion-st1-1"}}' >"$FIX/state.json"
pod_fixture legion-st1-2-architect-g1 architect ST1-2 1 Running >"$FIX/pod-legion-st1-2-architect-g1.json"
expect_ok tree-moved 'sub-architect on ST1-2 pod legion-st1-2-architect-g1 Running; no child is a tree of its own'
base_state | jq '.trees["ST1-2"] = {status:"queued",generation:0,launchFailures:0}' >"$FIX/state.json"
expect_failed tree-moved 'child ST1-2 is admitted as a tree of its own (LEGION-57)'
base_state | jq '.admission.queue = ["ST1-2"]' >"$FIX/state.json"
expect_failed tree-moved 'child ST1-2 is admitted as a tree of its own (LEGION-57)'
base_state >"$FIX/state.json"
expect_failed tree-moved 'no sub-architect on a child of ST1-1 and no phase worker on it holds a claim with a pod'
# a phase worker on a single-issue root
rm "$FIX/children-ST1-1.json"
issue_fixture ST1-1 in_progress art-1 >"$FIX/issue-ST1-1.json"
base_state | jq '.roles["legion-demo-st1-1-planner"] = {role:"planner",issue:"ST1-1",generation:1,sessionId:"p",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-1-planner-g1",podUid:"u2",pvcName:"legion-st1-1"}}' >"$FIX/state.json"
pod_fixture legion-st1-1-planner-g1 planner ST1-1 1 Pending >"$FIX/pod-legion-st1-1-planner-g1.json"
expect_failed tree-moved "claim legion-demo-st1-1-planner names pod legion-st1-1-planner-g1, which is 'Pending', not Running"
pod_fixture legion-st1-1-planner-g1 planner ST1-1 1 Running >"$FIX/pod-legion-st1-1-planner-g1.json"
expect_ok tree-moved 'phase worker planner on ST1-1 pod legion-st1-1-planner-g1 Running'
echo "checkpoints.test.sh: tree-moved OK"
echo "checkpoints.test.sh: OK"
