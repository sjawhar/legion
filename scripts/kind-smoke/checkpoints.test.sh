#!/usr/bin/env bash
# Harness for scripts/kind-smoke/checkpoints.sh: curl and kubectl are PATH fakes serving fixtures
# under $FIX (a NAME.json file, or NAME.seq — one fixture file name per line, served in order and
# the last one repeated), so every verdict is pinned without a cluster, a daemon, or a network.
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/kind-smoke/test-lib.sh
source "$here/test-lib.sh"
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
  *" exec "*" config --unset credential.interactive") exit "${FAKE_GIT_UNSET_EXIT:-0}" ;;
  *" exec "*" cat /proc/1/environ") n="${all#* exec }"; n="${n%% *}"; cat "$FIX/environ-$n" 2>/dev/null ;;
  *" exec "*"ls "*) n="${all#* exec }"; n="${n%% *}"; cat "$FIX/ls-$n" 2>/dev/null ;;
  *" logs "*) n="${all#* logs }"; n="${n%% *}"; cat "$FIX/logs-$n" 2>/dev/null || echo "(no log)" ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF

fake kind <<'EOF'
case "$*" in
  "get nodes --name legion-smoke-t1") echo legion-smoke-t1-control-plane ;;
  *) echo "unexpected kind request: $*" >&2; exit 1 ;;
esac
EOF
fake docker <<'EOF'
case "$*" in
  "exec legion-smoke-t1-control-plane crictl inspect "*) [ -n "${FAKE_CRICTL_FAIL:-}" ] && exit 1; echo 4242 ;;
  "exec legion-smoke-t1-control-plane kill -9 4242") exit 0 ;;
  *) echo "unexpected docker request: $*" >&2; exit 1 ;;
esac
EOF

state_dir="$tmp/state"
plant_records() { # plant_records [controller line] — the records up.sh writes
  rm -rf "$state_dir"
  mkdir -p "$state_dir/records" "$state_dir/logs" "$state_dir/pids" "$state_dir/overlay/secrets"
  mkdir -p "$state_dir/secrets" && chmod 0700 "$state_dir/secrets"
  local r
  for r in instance=t1 port-base=41000 gateway=172.30.0.1 project=demo dispatch-project=ST1 github-ingress=none session-store=pvc worker-cap=6 root-issue-count=1 repo=sjawhar/legion-smoke worker-idle-retire=600; do
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
  local status=0
  env SMOKE_DIR="$state_dir" SMOKE_INSTANCE=t1 SMOKE_POLL_INTERVAL=0 \
    SMOKE_WAIT_ADMITTED=1 SMOKE_WAIT_ARCHITECT_POD=1 SMOKE_WAIT_SPEC_POSTED=1 SMOKE_WAIT_TREE_MOVED=1 \
    SMOKE_WAIT_KILL_PHASE=3 SMOKE_WAIT_KILL_RESUME=3 SMOKE_WAIT_KILL_COMPLETE=3 SMOKE_WAIT_CAP_QUEUE=3 SMOKE_WAIT_CAP_PROMOTE=3 SMOKE_WAIT_DONE=1 \
    "$@" bash "$here/checkpoints.sh" "$name" >"$tmp/out.txt" 2>&1 || status=$?
  return $status
}
expect_verdict() { # expect_verdict OK|FAILED|SKIPPED-BLOCKED EXIT NAME 'substring' ENV…
  local verdict="$1" code="$2" name="$3" want="$4"
  shift 4
  local status=0
  run_cp "$name" "$@" || status=$?
  [ "$status" = "$code" ] || { echo "$name: expected exit $code, got $status" >&2; cat "$tmp/out.txt" >&2; exit 1; }
  [ "$(grep -c '^CHECKPOINT ' "$tmp/out.txt")" = 1 ] || { echo "$name: expected exactly one CHECKPOINT line" >&2; cat "$tmp/out.txt" >&2; exit 1; }
  refute grep -Ev '^(CHECKPOINT|WORKAROUND) ' "$tmp/out.txt"   # nothing but the verdict (and the one WORKAROUND line) is printed
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
    {metadata:{name:$n,labels:{"legion.dev/project":"demo","legion.dev/tree":"ST1-1","legion.dev/issue":$i,"legion.dev/role":$r,"legion.dev/generation":$g}},
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
[ "$status" = 1 ]
grep -Fq "error: record root-issues missing under $state_dir: run scripts/kind-smoke/up.sh first" "$tmp/out.txt"
status=0; run_cp not-a-checkpoint || status=$?
[ "$status" = 2 ]
grep -Fq 'usage: checkpoints.sh <admitted|architect-pod|' "$tmp/out.txt"
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

# ---- kill-pod-resume ----------------------------------------------------------------------------------
sess_file='/home/legion/.omp/profiles/legion/agent/sessions/--x--/2026-09-15T00-00-00-000Z_arch.jsonl'
kill_state() { # kill_state GEN POD READY(1|0) SESSION CLAIMS_JSON → a state document for the kill sequence
  base_state | jq --argjson g "$1" --arg p "$2" --arg ready "$3" --arg s "$4" --argjson claims "$5" '
    .trees["ST1-1"].generation = $g | .trees["ST1-1"].locator.podName = $p | .trees["ST1-1"].locator.podUid = ("u-" + $p)
    | (if $ready == "1" then . else del(.trees["ST1-1"].readyConfirmedAt) end)
    | .roles["legion-demo-st1-1-architect"].sessionId = $s | .roles["legion-demo-st1-1-architect"].generation = $g
    | .roles["legion-demo-st1-1-architect"].locator.podName = $p
    | .roles += $claims'
}
planner_claim='{"legion-demo-st1-1-planner":{"role":"planner","issue":"ST1-1","generation":1,"sessionId":"p","readyConfirmedAt":"2026-09-15T00:00:00Z","locator":{"runtime":"kubernetes","namespace":"legion","podName":"legion-st1-1-planner-g1","podUid":"up","pvcName":"legion-st1-1"}}}'
planner_and_implementer='{"legion-demo-st1-1-planner":{"role":"planner","issue":"ST1-1","generation":1,"sessionId":"p"},"legion-demo-st1-1-implementer":{"role":"implementer","issue":"ST1-1","generation":1,"sessionId":"i","locator":{"runtime":"kubernetes","namespace":"legion","podName":"legion-st1-1-implementer-g1","podUid":"ui","pvcName":"legion-st1-1"}}}'
plant_kill_fixtures() { # plant_kill_fixtures G2 SESSION1 RESUME_FILE — the four-read sequence of a clean resurrection
  reset_fixtures
  issue_fixture ST1-1 in_progress art-1 >"$FIX/issue-ST1-1.json"
  kill_state 1 legion-st1-1-architect-g1 1 arch "$planner_claim" >"$FIX/state-1.json"
  kill_state "$1" "legion-st1-1-architect-g$1" 0 arch "$planner_claim" >"$FIX/state-2.json"
  kill_state "$1" "legion-st1-1-architect-g$1" 1 "$2" "$planner_claim" >"$FIX/state-3.json"
  kill_state "$1" "legion-st1-1-architect-g$1" 1 "$2" "$planner_and_implementer" >"$FIX/state-4.json"
  printf 'state-1.json\nstate-2.json\nstate-3.json\nstate-4.json\n' >"$FIX/state.seq"
  pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running | jq '.status.containerStatuses = [{name:"worker",containerID:"containerd://abc123def456abc123def456"}]' >"$FIX/pod-legion-st1-1-architect-g1.json"
  pod_fixture "legion-st1-1-architect-g$1" architect ST1-1 "$1" Running small "$3" >"$FIX/pod-legion-st1-1-architect-g$1.json"
}
plant_records
plant_kill_fixtures 2 arch "$sess_file"
expect_ok kill-pod-resume "ST1-1 architect pod legion-st1-1-architect-g1 → legion-st1-1-architect-g2 generation 1→2 (kill: docker exec legion-smoke-t1-control-plane kill -9 4242 (container abc123def456); LEGION-177 workaround applied, keeper not running) session arch unchanged; --resume=$sess_file; tree moved after the replacement registered — claims changed:"
grep -Fxq 'WORKAROUND LEGION-177 applied' "$tmp/out.txt"
grep -Fq 'exec legion-st1-1-architect-g1 -c worker -- git --git-dir=/legion/repos/github.com/sjawhar/legion-smoke/.git config --unset credential.interactive' "$FAKE_LOG"
grep -Fq 'exec legion-smoke-t1-control-plane crictl inspect -o go-template --template {{.info.pid}} abc123def456abc123def456' "$FAKE_LOG"
refute grep -Fq 'delete pod' "$FAKE_LOG"
# the workaround is skipped on request; the kill falls back to a forced delete when crictl fails
: >"$FAKE_LOG"
plant_kill_fixtures 2 arch "$sess_file"
expect_ok kill-pod-resume '(kill: kubectl delete pod legion-st1-1-architect-g1 --grace-period=0 --force (fallback); LEGION-177 workaround off, keeper not running)' SMOKE_LEGION_177_WORKAROUND=0 FAKE_CRICTL_FAIL=1
grep -Fq 'WORKAROUND LEGION-177 skipped (SMOKE_LEGION_177_WORKAROUND=0)' "$tmp/out.txt"
refute grep -Fq 'config --unset credential.interactive' "$FAKE_LOG"
grep -Fq 'delete pod legion-st1-1-architect-g1 --grace-period=0 --force' "$FAKE_LOG"
# a key that was already unset counts as applied
plant_kill_fixtures 2 arch "$sess_file"
expect_ok kill-pod-resume 'LEGION-177 workaround applied (already unset)' FAKE_GIT_UNSET_EXIT=5
# generation jumped by two
plant_kill_fixtures 3 arch "$sess_file"
expect_failed kill-pod-resume 'generation advanced from 1 to 3, expected 2'
# the replacement resumes nothing
plant_kill_fixtures 2 arch ""
expect_failed kill-pod-resume 'replacement pod legion-st1-1-architect-g2 carries no --resume argument'
# the replacement resumes another file
plant_kill_fixtures 2 arch "/home/legion/.omp/profiles/legion/agent/sessions/--x--/other_zzz.jsonl"
expect_failed kill-pod-resume "--resume=/home/legion/.omp/profiles/legion/agent/sessions/--x--/other_zzz.jsonl does not name the recorded session file $sess_file"
# a different agent registered
plant_kill_fixtures 2 other-agent "$sess_file"
printf 'worker log line\n' >"$FIX/logs-legion-st1-1-architect-g2"
expect_failed kill-pod-resume "the replacement registered session 'other-agent', recorded arch (a different agent); worker log tail: worker log line"
# the tree moved on without a generation change: the kill did not land
plant_kill_fixtures 2 arch "$sess_file"
kill_state 1 legion-st1-1-architect-g1 1 arch "$planner_and_implementer" >"$FIX/state-2.json"
expect_failed kill-pod-resume 'the tree moved on without a generation change: recorded generation 1, current 1 — the kill (docker exec legion-smoke-t1-control-plane kill -9 4242 (container abc123def456)) did not land'
# the replacement's init container failed (LEGION-177 without the workaround)
plant_kill_fixtures 2 arch "$sess_file"
pod_fixture legion-st1-1-architect-g2 architect ST1-1 2 Pending small "$sess_file" | jq '.status.initContainerStatuses = [{name:"workspace-init",state:{terminated:{exitCode:1}}}]' >"$FIX/pod-legion-st1-1-architect-g2.json"
printf 'fatal: unable to get password from user\n' >"$FIX/logs-legion-st1-1-architect-g2"
expect_failed kill-pod-resume 'replacement pod legion-st1-1-architect-g2: its init container failed (LEGION-177 without the workaround? SMOKE_LEGION_177_WORKAROUND=0); workspace-init log tail: fatal: unable to get password from user' SMOKE_LEGION_177_WORKAROUND=0
# a change that landed before the replacement registered does not count as the tree moving afterwards
plant_kill_fixtures 2 arch "$sess_file"
kill_state 2 legion-st1-1-architect-g2 1 arch "$planner_and_implementer" >"$FIX/state-3.json"
expect_failed kill-pod-resume 'the tree of ST1-1 has not moved since the replacement registered (claims: implementer/ST1-1@1,planner/ST1-1@1; statuses: ST1-1 in_progress)'
# the tree must be mid-phase before the kill: no live worker claim → the wait times out and nothing
# is killed (the argv log is cleared first: the earlier cases above did kill)
plant_kill_fixtures 2 arch "$sess_file"
kill_state 1 legion-st1-1-architect-g1 1 arch '{}' >"$FIX/state-1.json"
printf 'state-1.json\n' >"$FIX/state.seq"
: >"$FAKE_LOG"
expect_failed kill-pod-resume 'no phase worker or sub-architect holds a claim with a pod on the tree of ST1-1 yet (the kill must land mid-phase)'
[ ! -f "$FIX/deleted" ]
refute grep -Fq 'kill -9' "$FAKE_LOG"
refute grep -Fq 'delete pod' "$FAKE_LOG"
# only the root architect is a supported target
expect_blocked kill-pod-resume 'SMOKE_KILL_ROLE=tester is not supported' SMOKE_KILL_ROLE=tester
echo "checkpoints.test.sh: kill-pod-resume OK"

# ---- pod-hygiene ----------------------------------------------------------------------------------------
plant_records
reset_fixtures
daemon_pod='{"metadata":{"name":"legion-daemon-demo-abc","labels":{"app.kubernetes.io/name":"legion-daemon","app.kubernetes.io/instance":"demo"}},"spec":{"containers":[{"name":"daemon","command":["legion","start","demo"],"env":[{"name":"DISPATCH_TOKEN","valueFrom":{"secretKeyRef":{"name":"legion-demo-providers","key":"DISPATCH_TOKEN"}}}]}]},"status":{"phase":"Running"}}'
pods_fixture() { jq -n --argjson a "$1" --argjson b "$2" --argjson d "$daemon_pod" '{items:[$a,$b,$d]}'; }
arch_pod="$(pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running small)"
tester_pod="$(pod_fixture legion-st1-1-tester-g1 tester ST1-1 1 Running large)"
pods_fixture "$arch_pod" "$tester_pod" >"$FIX/pods.json"
echo '{"spec":{"template":{"metadata":{"labels":{"app.kubernetes.io/name":"legion-daemon"}}}}}' >"$FIX/deploy.json"
printf 'PATH=/usr/bin\nLEGION_BOOT_TOKEN_FILE=/var/run/legion/boot/LEGION_BOOT_TOKEN\nHOME=/home/legion\n' >"$FIX/environ-legion-st1-1-architect-g1"
cp "$FIX/environ-legion-st1-1-architect-g1" "$FIX/environ-legion-st1-1-tester-g1"
expect_ok pod-hygiene '2 pods checked; profiles match; no secret in env/command/args; PID 1 clean; daemon unlabelled'
# a secret value planted in an env value: the reason names pod, container, and variable — never the value
pods_fixture "$arch_pod" "$(printf '%s' "$tester_pod" | jq '.spec.containers[0].env += [{name:"DISPATCH_TOKEN_CANARY",value:"dispatch-secret-value-0123456789"}]')" >"$FIX/pods.json"
expect_failed pod-hygiene 'pod legion-st1-1-tester-g1 container worker env DISPATCH_TOKEN_CANARY contains a secret value'
refute grep -Fq 'dispatch-secret-value-0123456789' "$tmp/out.txt"
# the provider key in a command element
pods_fixture "$arch_pod" "$(printf '%s' "$tester_pod" | jq '.spec.containers[0].command += ["--key=anthropic-canary-value"]')" >"$FIX/pods.json"
expect_failed pod-hygiene 'pod legion-st1-1-tester-g1 container worker command contains a secret value'
refute grep -Fq 'anthropic-canary-value' "$tmp/out.txt"
# PID 1 carries a provider key
pods_fixture "$arch_pod" "$tester_pod" >"$FIX/pods.json"
printf 'PATH=/usr/bin\nANTHROPIC_API_KEY=whatever\n' >"$FIX/environ-legion-st1-1-tester-g1"
expect_failed pod-hygiene 'pod legion-st1-1-tester-g1 PID 1 environment carries ANTHROPIC_API_KEY'
cp "$FIX/environ-legion-st1-1-architect-g1" "$FIX/environ-legion-st1-1-tester-g1"
# a profile mismatch names the field and both values
pods_fixture "$arch_pod" "$(printf '%s' "$tester_pod" | jq '.spec.containers[0].resources.requests.memory = "3Gi"')" >"$FIX/pods.json"
expect_failed pod-hygiene 'pod legion-st1-1-tester-g1 container worker requests.memory is 3Gi, profile large says 4Gi'
# the daemon pod must not carry the Legion label
pods_fixture "$arch_pod" "$tester_pod" | jq '.items[2].metadata.labels["legion.dev/project"] = "demo"' >"$FIX/pods.json"
expect_failed pod-hygiene 'the daemon pod carries legion.dev/project'
pods_fixture "$arch_pod" "$tester_pod" >"$FIX/pods.json"
echo '{"items":[]}' >"$FIX/pods.json"
expect_failed pod-hygiene 'no legion.dev/project pods are running; run architect-pod first'
echo "checkpoints.test.sh: pod-hygiene OK"

# ---- worker-cap ------------------------------------------------------------------------------------------
plant_records
reset_fixtures
expect_blocked worker-cap 'worker-cap needs a run started with SMOKE_ROOT_ISSUES=2 SMOKE_WORKER_CAP=1 (this run: SMOKE_ROOT_ISSUES=1 SMOKE_WORKER_CAP=6)'
echo 2 >"$state_dir/records/root-issue-count"
echo 1 >"$state_dir/records/worker-cap"
printf 'ST1-1\nST1-2\n' >"$state_dir/records/root-issues"
arch1="$(pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running small)"
arch2="$(pod_fixture legion-st1-2-architect-g1 architect ST1-2 1 Running small | jq '.metadata.labels["legion.dev/tree"] = "ST1-2"')"
planner1="$(pod_fixture legion-st1-1-planner-g1 planner ST1-1 1 Running small)"
planner2="$(pod_fixture legion-st1-2-planner-g1 planner ST1-2 1 Running small | jq '.metadata.labels["legion.dev/tree"] = "ST1-2"')"
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson c "$planner1" '{items:[$a,$b,$c]}' >"$FIX/pods-1.json"
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson c "$planner2" '{items:[$a,$b,$c]}' >"$FIX/pods-2.json"
printf 'pods-1.json\npods-2.json\n' >"$FIX/pods.seq"
base_state | jq '.workerAdmission.queue = [{roleToken:"legion-demo-st1-2-planner",issue:"ST1-2",role:"planner",kind:"assignment",queuedAt:"2026-09-15T00:00:00Z"}]' >"$FIX/state-1.json"
base_state | jq '.roles["legion-demo-st1-2-planner"] = {role:"planner",issue:"ST1-2",generation:1,sessionId:"p2",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-2-planner-g1",podUid:"u",pvcName:"legion-st1-2"}}' >"$FIX/state-2.json"
printf 'state-1.json\nstate-2.json\n' >"$FIX/state.seq"
printf '%s' "$planner2" >"$FIX/pod-legion-st1-2-planner-g1.json"
expect_ok worker-cap 'queue held ST1-2/planner while 1 worker pod(s) ran; promoted to pod legion-st1-2-planner-g1; worker pods peaked at 1 with worker_cap 1 (an excess is idle lingering, allowed up to worker_idle_retire_seconds 600 + 30s; longest 0s), sampled every 0s'
# two worker pods alive at once with cap 1: idle lingering within the allowance is reported, not failed
reset_fixtures
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson c "$planner1" --argjson d "$planner2" '{items:[$a,$b,$c,$d]}' >"$FIX/pods-1.json"
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson d "$planner2" '{items:[$a,$b,$d]}' >"$FIX/pods-2.json"
printf 'pods-1.json\npods-2.json\n' >"$FIX/pods.seq"
printf 'state-1.json\nstate-2.json\n' >"$FIX/state.seq"
base_state | jq '.workerAdmission.queue = [{roleToken:"legion-demo-st1-2-planner",issue:"ST1-2",role:"planner"}]' >"$FIX/state-1.json"
base_state | jq '.roles["legion-demo-st1-2-planner"] = {role:"planner",issue:"ST1-2",generation:1,sessionId:"p2",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-2-planner-g1",podUid:"u",pvcName:"legion-st1-2"}}' >"$FIX/state-2.json"
printf '%s' "$planner2" >"$FIX/pod-legion-st1-2-planner-g1.json"
expect_ok worker-cap 'queue held ST1-2/planner while 2 worker pod(s) ran; promoted to pod legion-st1-2-planner-g1; worker pods peaked at 2 with worker_cap 1 (an excess is idle lingering, allowed up to worker_idle_retire_seconds 600 + 30s; longest 0s)'
# an excess that outlasts the idle allowance is a violation (allowance 1 + 30 s; the excess is held across the whole wait)
echo 1 >"$state_dir/records/worker-idle-retire"
reset_fixtures
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson c "$planner1" --argjson d "$planner2" '{items:[$a,$b,$c,$d]}' >"$FIX/pods.json"
base_state | jq '.workerAdmission.queue = []' >"$FIX/state.json"
expect_failed worker-cap 'worker pods have run for' SMOKE_WAIT_CAP_QUEUE=40 SMOKE_POLL_INTERVAL=1
grep -Fq 'with worker_cap 1, longer than idle lingering (worker_idle_retire_seconds 1 + 30s) can explain: legion-st1-1-planner-g1,legion-st1-2-planner-g1' "$tmp/out.txt"
# a pod already being deleted is not counted
echo 600 >"$state_dir/records/worker-idle-retire"
reset_fixtures
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson c "$(printf '%s' "$planner1" | jq '.metadata.deletionTimestamp = "2026-09-15T00:00:00Z"')" --argjson d "$planner2" '{items:[$a,$b,$c,$d]}' >"$FIX/pods.json"
base_state | jq '.workerAdmission.queue = [{roleToken:"legion-demo-st1-2-implementer",issue:"ST1-2",role:"implementer"}]' >"$FIX/state-1.json"
base_state | jq '.roles["legion-demo-st1-2-implementer"] = {role:"implementer",issue:"ST1-2",generation:1,sessionId:"i2",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-2-planner-g1",podUid:"u",pvcName:"legion-st1-2"}}' >"$FIX/state-2.json"
printf 'state-1.json\nstate-2.json\n' >"$FIX/state.seq"
printf '%s' "$planner2" >"$FIX/pod-legion-st1-2-planner-g1.json"
expect_ok worker-cap 'queue held ST1-2/implementer while 1 worker pod(s) ran'
echo "checkpoints.test.sh: worker-cap OK"

# ---- done ----------------------------------------------------------------------------------------------------
plant_records
reset_fixtures
expect_blocked "done" 'the run has no controller (the checkout has no legion controller start (pull request #1110))'
plant_records 'tmux legion-smoke-t1 controller'
expect_blocked "done" 'the run has SMOKE_GITHUB_INGRESS=none: merges need GitHub events'
echo envoy >"$state_dir/records/github-ingress"
# a PATH holding the fakes and only the coreutils the script needs — CI runners have gh in /usr/bin
nogh="$tmp/bin-nogh"
mkdir -p "$nogh"
cp "$fake_bin"/* "$nogh/"
for t in bash jq awk cat tr sed head tail paste sort diff grep wc cut sleep mkdir chmod date seq env; do
  [ -e "$nogh/$t" ] || ln -s "$(command -v "$t")" "$nogh/$t"
done
expect_blocked "done" 'gh is not on PATH: needed to confirm the pull requests merged' PATH="$nogh"
echo "checkpoints.test.sh: done OK"
echo "checkpoints.test.sh: OK"
