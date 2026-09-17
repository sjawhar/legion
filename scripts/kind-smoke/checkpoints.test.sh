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
    # shellcheck disable=SC2016  # The generated fake expands these variables when it runs.
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
# next_fixture NAME → the next entry of NAME.seq (advancing it; the last one repeated), else NAME.json, else {}
cat >"$fake_bin/next_fixture" <<'EOF'
#!/usr/bin/env bash
name="$1"
if [ -f "$FIX/$name.seq" ]; then
  n="$(cat "$FIX/$name.counter" 2>/dev/null || echo 0)"; n=$((n + 1)); echo "$n" >"$FIX/$name.counter"
  total="$(wc -l <"$FIX/$name.seq")"; [ "$n" -gt "$total" ] && n="$total"
  sed -n "${n}p" "$FIX/$name.seq"
elif [ -f "$FIX/$name.json" ]; then echo "$name.json"
else echo "{}"; fi
EOF
chmod +x "$fake_bin/next_fixture"
cat >"$fake_bin/serve_file" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "{}" ]; then echo '{}'; else cat "$FIX/$1"; fi
EOF
chmod +x "$fake_bin/serve_file"
fake curl <<'EOF'
url=""; for a in "$@"; do case "$a" in http*) url="$a" ;; esac; done
path="${url#*://*/}"
case "$path" in
  legion/v1/state) serve state ;;
  api/v1/issues/*/asks*) k="${path#api/v1/issues/}"; k="${k%%/*}"; [ -f "$FIX/asks-$k.json" ] && cat "$FIX/asks-$k.json" || echo '[]' ;;
  api/v1/issues/*)
    k="${path#api/v1/issues/}"
    # $FIX/fail-once-issue-<KEY>: the next read fails (curl 7, connection refused) and the marker is consumed;
    # $FIX/fail-always-issue-<KEY>: every read fails — Dispatch down
    if [ -f "$FIX/fail-once-issue-$k" ]; then rm -f "$FIX/fail-once-issue-$k"; echo "curl: (7) Failed to connect" >&2; exit 7; fi
    if [ -f "$FIX/fail-always-issue-$k" ]; then echo "curl: (7) Failed to connect" >&2; exit 7; fi
    serve "issue-$k" ;;
  api/v1/issues\?*parent=*) k="${path##*parent=}"; k="${k%%&*}"; [ -f "$FIX/children-$k.json" ] && cat "$FIX/children-$k.json" || echo '[]' ;;
  *) echo "unexpected curl request: $*" >&2; exit 1 ;;
esac
EOF
fake kubectl <<'EOF'
all="$*"
case "$all" in
  *" get secret "*" -o json") n="${all#* get secret }"; n="${n%% *}"; serve "secret-$n" ;;
  *" get pod "*" -o json")
    n="${all#* get pod }"; n="${n%% *}"
    # a sequence entry named notfound / blip stands in for kubectl's two failure shapes
    next="$(next_fixture "pod-$n")"
    case "$next" in
      notfound) echo "Error from server (NotFound): pods \"$n\" not found" >&2; exit 1 ;;
      blip) echo "Unable to connect to the server: dial tcp 127.0.0.1:41004: connect: connection refused" >&2; exit 1 ;;
      *) serve_file "$next" ;;
    esac ;;
  *" get pvc "*" -o json") n="${all#* get pvc }"; n="${n%% *}"; serve "pvc-$n" ;;
  *" get pods "*"-o json") serve pods ;;
  *" get deploy "*"-o json") serve deploy ;;
  *" delete pods -l legion.dev/tree="*) echo "deleted-tree-pods" >>"$FIX/deleted" ;;
  *" delete pod "*) n="${all#* delete pod }"; n="${n%% *}"; echo "deleted-$n" >>"$FIX/deleted" ;;
  *" delete pvc "*) n="${all#* delete pvc }"; n="${n%% *}"; echo "deleted-pvc-$n" >>"$FIX/deleted" ;;
  *" exec deploy/legion-daemon-demo -c daemon -- cat /var/lib/legion/state.json") cat "$FIX/raw-state.json" ;;
  *" exec "*" config --unset credential.interactive") exit "${FAKE_GIT_UNSET_EXIT:-0}" ;;
  *" exec "*" cat /legion/workspace/.legion/workspace-recovered.json") n="${all#* exec }"; n="${n%% *}"; cat "$FIX/recovered-$n.json" ;;
  *" exec "*" cat /proc/1/environ") n="${all#* exec }"; n="${n%% *}"; cat "$FIX/environ-$n" 2>/dev/null ;;
  *" exec "*"ls "*) n="${all#* exec }"; n="${n%% *}"; cat "$FIX/ls-$n" 2>/dev/null ;;
  *" logs "*) n="${all#* logs }"; n="${n%% *}"; cat "$FIX/logs-$n" 2>/dev/null || echo "(no log)" ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF

fake gh <<'EOF'
[ -f "$FIX/gh-fails" ] && { echo "HTTP 401: Bad credentials" >&2; exit 1; }
case "$*" in
  "pr list --repo sjawhar/legion-smoke --limit 1 --json number") echo '[]' ;;
  *"--state merged --search head:legion/"*) echo "" ;;
  *) echo "unexpected gh request: $*" >&2; exit 1 ;;
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
fake tmux <<'EOF'
case "$*" in
  *"list-panes -a -F #{pane_current_command}"*) cat "$FIX/tmux-panes" 2>/dev/null ;;
  *"pipe-pane -t "*) ;;
  *) echo "unexpected tmux request: $*" >&2; exit 1 ;;
esac
EOF
fake omp <<'EOF'
case "$*" in
  plugin\ install\ *) echo "installed ${*: -1}" ;;
  *) echo "unexpected omp request: $*" >&2; exit 1 ;;
esac
EOF
fake daemon-ctl.sh <<'EOF'
echo "$1" >>"$FIX/daemon-ctl-calls"
EOF

state_dir="$tmp/state"
plant_records() { # plant_records [controller line] — the records up.sh writes
  rm -rf "$state_dir"
  mkdir -p "$state_dir/records" "$state_dir/logs" "$state_dir/pids" "$state_dir/overlay/secrets"
  mkdir -p "$state_dir/secrets" && chmod 0700 "$state_dir/secrets"
  local r
  for r in instance=t1 port-base=41000 gateway=172.30.0.1 project=demo dispatch-project=ST1 github-ingress=none session-store=pvc daemon-mode=cluster worker-cap=6 root-issue-count=1 repo=sjawhar/legion-smoke worker-idle-retire=600; do
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
run_cp() { # run_cp NAME [ENV…] [FLAG…] → $tmp/out.txt, exit code returned
  local name="$1" arg status=0
  local env_args=() flag_args=()
  shift
  for arg in "$@"; do
    case "$arg" in *=*) env_args+=("$arg") ;; *) flag_args+=("$arg") ;; esac
  done
  env SMOKE_DIR="$state_dir" SMOKE_INSTANCE=t1 SMOKE_POLL_INTERVAL=0 \
    SMOKE_WAIT_ADMITTED=1 SMOKE_WAIT_ARCHITECT_POD=1 SMOKE_WAIT_SPEC_POSTED=1 SMOKE_WAIT_TREE_MOVED=1 \
    SMOKE_WAIT_KILL_PHASE=3 SMOKE_WAIT_KILL_RESUME=3 SMOKE_WAIT_KILL_COMPLETE=3 SMOKE_WAIT_CAP_QUEUE=3 SMOKE_WAIT_CAP_PROMOTE=3 SMOKE_WAIT_DONE=1 \
    "${env_args[@]}" bash "$here/checkpoints.sh" "$name" "${flag_args[@]}" >"$tmp/out.txt" 2>&1 || status=$?
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
# one transient Dispatch failure on the issue read: the poll retries and exactly one OK line is printed
issue_fixture ST1-1 in_progress >"$FIX/issue-ST1-1.json"
touch "$FIX/fail-once-issue-ST1-1"
expect_ok admitted 'ST1-1 status=in_progress tree=active controller=external'
[ ! -f "$FIX/fail-once-issue-ST1-1" ]
# Dispatch down throughout: exactly one FAILED line, naming Dispatch
touch "$FIX/fail-always-issue-ST1-1"
expect_failed admitted "Dispatch did not answer GET /api/v1/issues/ST1-1 (see $state_dir/logs/dispatch.log)"
rm -f "$FIX/fail-always-issue-ST1-1"
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
touch "$FIX/fail-once-issue-ST1-1"
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
kill_assigned_at="$(date -u -d '60 seconds ago' +%FT%TZ)"
active_implementer="$(jq -cn --arg at "$kill_assigned_at" '{phase:"implementer",sessionId:"i",assignedAt:$at}')"
kill_state() { # kill_state GEN POD READY(1|0) SESSION CLAIMS_JSON [PHASE_JSON] → a state document for the kill sequence
  local active_phase="${6:-$active_implementer}"
  base_state | jq --argjson g "$1" --arg p "$2" --arg ready "$3" --arg s "$4" --argjson claims "$5" --argjson phase "$active_phase" '
    .trees["ST1-1"].generation = $g | .trees["ST1-1"].locator.podName = $p | .trees["ST1-1"].locator.podUid = ("u-" + $p)
    | (if $ready == "1" then . else del(.trees["ST1-1"].readyConfirmedAt) end)
    | .roles["legion-demo-st1-1-architect"].sessionId = $s | .roles["legion-demo-st1-1-architect"].generation = $g
    | .roles["legion-demo-st1-1-architect"].locator.podName = $p
    | .roles += $claims | .phases["ST1-1"] = $phase'
}
planner_and_implementer='{"legion-demo-st1-1-planner":{"role":"planner","issue":"ST1-1","generation":1,"sessionId":"p"},"legion-demo-st1-1-implementer":{"role":"implementer","issue":"ST1-1","generation":1,"sessionId":"i","readyConfirmedAt":"2026-09-15T00:00:00Z","locator":{"runtime":"kubernetes","namespace":"legion","podName":"legion-st1-1-implementer-g1","podUid":"ui","pvcName":"legion-st1-1"}}}'
moved_claims="$(printf '%s' "$planner_and_implementer" | jq '. + {"legion-demo-st1-1-tester": {role:"tester", issue:"ST1-1", generation:1, sessionId:"t"}}')"
redact_phase_state() {
  cp "$FIX/state-1.json" "$FIX/raw-state.json"
  jq 'del(.phases)' "$FIX/raw-state.json" >"$FIX/state-1.public" && mv "$FIX/state-1.public" "$FIX/state-1.json"
}
plant_kill_fixtures() { # plant_kill_fixtures G2 SESSION1 RESUME_FILE — the four-read sequence of a clean resurrection
  reset_fixtures
  issue_fixture ST1-1 in_progress art-1 >"$FIX/issue-ST1-1.json"
  kill_state 1 legion-st1-1-architect-g1 1 arch "$planner_and_implementer" >"$FIX/state-1.json"
  kill_state "$1" "legion-st1-1-architect-g$1" 0 arch "$planner_and_implementer" >"$FIX/state-2.json"
  kill_state "$1" "legion-st1-1-architect-g$1" 1 "$2" "$planner_and_implementer" >"$FIX/state-3.json"
  kill_state "$1" "legion-st1-1-architect-g$1" 1 "$2" "$moved_claims" >"$FIX/state-4.json"
  printf 'state-1.json\nstate-2.json\nstate-3.json\nstate-4.json\n' >"$FIX/state.seq"
  redact_phase_state
  pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running | jq '.status.containerStatuses = [{name:"worker",containerID:"containerd://abc123def456abc123def456"}]' >"$FIX/pod-g1-running.json"
  pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Failed | jq '.status.containerStatuses = [{name:"worker",containerID:"containerd://abc123def456abc123def456",state:{terminated:{exitCode:137,reason:"Error"}}}]' >"$FIX/pod-g1-failed.json"
  # reads of pod0: the target poll, the containerID for the kill, then the landed poll sees it Failed
  printf 'pod-g1-running.json\npod-g1-running.json\npod-g1-failed.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
  pod_fixture "legion-st1-1-architect-g$1" architect ST1-1 "$1" Running small "$3" >"$FIX/pod-legion-st1-1-architect-g$1.json"
  pod_fixture legion-st1-1-implementer-g1 implementer ST1-1 1 Running >"$FIX/pod-legion-st1-1-implementer-g1.json"
  printf 'agent_start\ntool_execution_start bash {}\n' >"$FIX/logs-legion-st1-1-implementer-g1"
}
# The root crash is deliberately restricted to an early, active implementer turn. A future change
# that treats a planner or an idle/stale implementer as killable must fail these cases.
plant_records
plant_kill_fixtures 2 arch "$sess_file"
kill_state 1 legion-st1-1-architect-g1 1 arch "$planner_and_implementer" "$(jq -cn --arg at "$kill_assigned_at" '{phase:"planner",sessionId:"p",assignedAt:$at}')" >"$FIX/state-1.json"
redact_phase_state
printf 'state-1.json\n' >"$FIX/state.seq"
printf 'pod-g1-running.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
expect_failed kill-pod-resume "active phase is planner, expected implementer"
refute grep -Fq 'kill -9' "$FAKE_LOG"

plant_kill_fixtures 2 arch "$sess_file"
kill_state 1 legion-st1-1-architect-g1 1 arch "$planner_and_implementer" "$(jq -cn --arg at "$(date -u -d '121 seconds ago' +%FT%TZ)" '{phase:"implementer",sessionId:"i",assignedAt:$at}')" >"$FIX/state-1.json"
redact_phase_state
printf 'state-1.json\n' >"$FIX/state.seq"
printf 'pod-g1-running.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
expect_failed kill-pod-resume "expected under 120s"
refute grep -Fq 'kill -9' "$FAKE_LOG"

plant_kill_fixtures 2 arch "$sess_file"
jq 'del(.roles["legion-demo-st1-1-implementer"].readyConfirmedAt)' "$FIX/raw-state.json" >"$FIX/state-1.json"
redact_phase_state
printf 'state-1.json\n' >"$FIX/state.seq"
printf 'pod-g1-running.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
expect_failed kill-pod-resume "implementer claim is not ready-confirmed"
refute grep -Fq 'kill -9' "$FAKE_LOG"

plant_kill_fixtures 2 arch "$sess_file"
printf 'agent_start\nagent_end\n' >"$FIX/logs-legion-st1-1-implementer-g1"
printf 'state-1.json\n' >"$FIX/state.seq"
printf 'pod-g1-running.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
expect_failed kill-pod-resume "implementer has no mid-task tool activity"
refute grep -Fq 'kill -9' "$FAKE_LOG"

plant_records
plant_kill_fixtures 2 arch "$sess_file"
expect_ok kill-pod-resume "ST1-1 architect pod legion-st1-1-architect-g1 → legion-st1-1-architect-g2 generation 1→2 (kill: docker exec legion-smoke-t1-control-plane kill -9 4242 (container abc123def456), landed: pod legion-st1-1-architect-g1 Failed (worker exit 137); LEGION-177 workaround applied, keeper not running) session arch unchanged; --resume=$sess_file; tree moved after the replacement registered — claims changed:"
grep -Fxq 'WORKAROUND LEGION-177 applied' "$tmp/out.txt"
grep -Fq 'exec legion-st1-1-architect-g1 -c worker -- git --git-dir=/legion/repos/github.com/sjawhar/legion-smoke/.git config --unset credential.interactive' "$FAKE_LOG"
grep -Fq 'exec legion-smoke-t1-control-plane crictl inspect -o go-template --template {{.info.pid}} abc123def456abc123def456' "$FAKE_LOG"
refute grep -Fq 'delete pod' "$FAKE_LOG"
# the workaround is skipped on request; the kill falls back to a forced delete when crictl fails
: >"$FAKE_LOG"
plant_kill_fixtures 2 arch "$sess_file"
expect_ok kill-pod-resume '(kill: kubectl delete pod legion-st1-1-architect-g1 --grace-period=0 --force (fallback), landed: pod legion-st1-1-architect-g1 Failed (worker exit 137); LEGION-177 workaround off, keeper not running)' SMOKE_LEGION_177_WORKAROUND=0 FAKE_CRICTL_FAIL=1
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
# pod0 died and a worker finished meanwhile (the tree moved at the recorded generation): other work continuing, never
# an immediate verdict — the poll keeps going and only the budget decides, naming both generations
plant_kill_fixtures 2 arch "$sess_file"
kill_state 1 legion-st1-1-architect-g1 1 arch "$moved_claims" >"$FIX/state-2.json"
printf 'state-1.json\nstate-2.json\n' >"$FIX/state.seq"
expect_failed kill-pod-resume 'no replacement within 3s: the daemon did not resurrect the root (recorded generation 1, current 1; pod0 pod legion-st1-1-architect-g1 Failed (worker exit 137), locator legion-st1-1-architect-g1'
refute grep -Fq 'did not land' "$tmp/out.txt"
# the same movement while pod0 is Running stays the immediate kill-did-not-land verdict (below)
# pod0 stays Running while the tree moves on: the kill did not land, judged from the pod, not from the tree
plant_kill_fixtures 2 arch "$sess_file"
printf 'pod-g1-running.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
kill_state 1 legion-st1-1-architect-g1 1 arch "$moved_claims" >"$FIX/state-2.json"
expect_failed kill-pod-resume 'the kill did not land: pod legion-st1-1-architect-g1 is still Running after docker exec legion-smoke-t1-control-plane kill -9 4242 (container abc123def456) while the tree moved on at generation 1'
# a kubectl failure reading pod0 is never "gone": an API blip is retried within the budget and named at expiry
plant_kill_fixtures 2 arch "$sess_file"
printf 'pod-g1-running.json\npod-g1-running.json\nblip\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
expect_failed kill-pod-resume 'could not read pod legion-st1-1-architect-g1: Unable to connect to the server: dial tcp 127.0.0.1:41004: connect: connection refused'
refute grep -Fq 'gone' "$tmp/out.txt"
# a blip followed by the pod's death is landed: the retry read it
plant_kill_fixtures 2 arch "$sess_file"
printf 'pod-g1-running.json\npod-g1-running.json\nblip\npod-g1-failed.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
expect_ok kill-pod-resume 'landed: pod legion-st1-1-architect-g1 Failed (worker exit 137)'
# the forced-delete fallback: pod0 is gone afterwards, which counts as landed
plant_kill_fixtures 2 arch "$sess_file"
printf 'pod-g1-running.json\npod-g1-running.json\nnotfound\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"
expect_ok kill-pod-resume 'landed: pod legion-st1-1-architect-g1 gone; LEGION-177 workaround off' SMOKE_LEGION_177_WORKAROUND=0 FAKE_CRICTL_FAIL=1
# a transient Dispatch failure while the kill-time tree snapshot is read is retried, never a verdict
# (the state read of that pass is spent, so the sequence serves the mid-phase state once more)
plant_kill_fixtures 2 arch "$sess_file"
printf 'state-1.json\nstate-1.json\nstate-2.json\nstate-3.json\nstate-4.json\n' >"$FIX/state.seq"
touch "$FIX/fail-once-issue-ST1-1"
expect_ok kill-pod-resume 'generation 1→2'
[ ! -f "$FIX/fail-once-issue-ST1-1" ]
# the replacement's init container failed (LEGION-177 without the workaround)
plant_kill_fixtures 2 arch "$sess_file"
pod_fixture legion-st1-1-architect-g2 architect ST1-1 2 Pending small "$sess_file" | jq '.status.initContainerStatuses = [{name:"workspace-init",state:{terminated:{exitCode:1}}}]' >"$FIX/pod-legion-st1-1-architect-g2.json"
printf 'fatal: unable to get password from user\n' >"$FIX/logs-legion-st1-1-architect-g2"
expect_failed kill-pod-resume 'replacement pod legion-st1-1-architect-g2: its init container failed (LEGION-177 without the workaround? SMOKE_LEGION_177_WORKAROUND=0); workspace-init log tail: fatal: unable to get password from user' SMOKE_LEGION_177_WORKAROUND=0
# a change that landed before the replacement registered does not count as the tree moving afterwards
plant_kill_fixtures 2 arch "$sess_file"
kill_state 2 legion-st1-1-architect-g2 1 arch "$planner_and_implementer" >"$FIX/state-3.json"
kill_state 2 legion-st1-1-architect-g2 1 arch "$planner_and_implementer" >"$FIX/state-4.json"
expect_failed kill-pod-resume 'the tree of ST1-1 has not moved since the replacement registered (claims: implementer/ST1-1@1,planner/ST1-1@1; statuses: ST1-1 in_progress)'
# the tree must be mid-phase before the kill: no live worker claim → the wait times out and nothing
# is killed (the argv log is cleared first: the earlier cases above did kill)
plant_kill_fixtures 2 arch "$sess_file"
kill_state 1 legion-st1-1-architect-g1 1 arch '{}' >"$FIX/state-1.json"
printf 'state-1.json\n' >"$FIX/state.seq"
printf 'pod-g1-running.json\n' >"$FIX/pod-legion-st1-1-architect-g1.seq"   # nothing kills it: it stays Running
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
# a failed kubectl exec (no environ read) is FAILED naming the pod — never "PID 1 clean"
pods_fixture "$arch_pod" "$tester_pod" >"$FIX/pods.json"
mv "$FIX/environ-legion-st1-1-tester-g1" "$FIX/environ-saved"
expect_failed pod-hygiene 'could not read the PID 1 environment of pod legion-st1-1-tester-g1 (kubectl exec failed); nothing is claimed clean for it'
mv "$FIX/environ-saved" "$FIX/environ-legion-st1-1-tester-g1"
# an exec that answers an empty environment is not clean either
: >"$FIX/environ-legion-st1-1-tester-g1"
expect_failed pod-hygiene 'pod legion-st1-1-tester-g1 PID 1 environment read back empty'
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
# one sample of queue-held-with-nothing-running (a retired pod deleted before the promoted one exists) is tolerated
reset_fixtures
jq -n --argjson a "$arch1" --argjson b "$arch2" '{items:[$a,$b]}' >"$FIX/pods-0.json"
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson d "$planner2" '{items:[$a,$b,$d]}' >"$FIX/pods-1.json"
printf 'pods-0.json\npods-1.json\n' >"$FIX/pods.seq"
base_state | jq '.workerAdmission.queue = [{roleToken:"legion-demo-st1-2-planner",issue:"ST1-2",role:"planner"}]' >"$FIX/state-1.json"
base_state | jq '.roles["legion-demo-st1-2-planner"] = {role:"planner",issue:"ST1-2",generation:1,sessionId:"p2",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-2-planner-g1",podUid:"u",pvcName:"legion-st1-2"}}' >"$FIX/state-2.json"
printf 'state-1.json\nstate-1.json\nstate-2.json\n' >"$FIX/state.seq"
printf '%s' "$planner2" >"$FIX/pod-legion-st1-2-planner-g1.json"
expect_ok worker-cap 'queue held ST1-2/planner while 1 worker pod(s) ran; promoted to pod legion-st1-2-planner-g1'
# a zero-running sample, an empty queue, then a zero-running sample again: not consecutive — no violation, the
# run goes on to be promoted
reset_fixtures
jq -n --argjson a "$arch1" --argjson b "$arch2" '{items:[$a,$b]}' >"$FIX/pods-0.json"
jq -n --argjson a "$arch1" --argjson b "$arch2" --argjson d "$planner2" '{items:[$a,$b,$d]}' >"$FIX/pods-1.json"
# ticks (each reads state, then pods): held/0 → empty/0 → held/0 → held/1 (queued while a pod runs) → promoted
printf 'pods-0.json\npods-0.json\npods-0.json\npods-1.json\npods-1.json\n' >"$FIX/pods.seq"
base_state | jq '.workerAdmission.queue = [{roleToken:"legion-demo-st1-2-planner",issue:"ST1-2",role:"planner"}]' >"$FIX/state-held.json"
base_state | jq '.workerAdmission.queue = []' >"$FIX/state-empty.json"
base_state | jq '.roles["legion-demo-st1-2-planner"] = {role:"planner",issue:"ST1-2",generation:1,sessionId:"p2",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-2-planner-g1",podUid:"u",pvcName:"legion-st1-2"}}' >"$FIX/state-2.json"
printf 'state-held.json\nstate-empty.json\nstate-held.json\nstate-held.json\nstate-2.json\n' >"$FIX/state.seq"
printf '%s' "$planner2" >"$FIX/pod-legion-st1-2-planner-g1.json"
expect_ok worker-cap 'promoted to pod legion-st1-2-planner-g1' SMOKE_WAIT_CAP_QUEUE=6
# two consecutive samples are the violation
reset_fixtures
jq -n --argjson a "$arch1" --argjson b "$arch2" '{items:[$a,$b]}' >"$FIX/pods.json"
base_state | jq '.workerAdmission.queue = [{roleToken:"legion-demo-st1-2-planner",issue:"ST1-2",role:"planner"}]' >"$FIX/state.json"
expect_failed worker-cap 'the worker queue held a task while no worker pod ran for 2 consecutive samples'
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
: >"$FAKE_LOG"
expect_blocked "done" 'the run has no controller (the checkout has no legion controller start (pull request #1110))'
plant_records 'tmux legion-smoke-t1 controller'
expect_blocked "done" 'the run has SMOKE_GITHUB_INGRESS=none: merges need GitHub events'
echo envoy >"$state_dir/records/github-ingress"
# a PATH holding the fakes and only the coreutils the script needs — CI runners have gh in /usr/bin
nogh="$tmp/bin-nogh"
mkdir -p "$nogh"
cp "$fake_bin"/* "$nogh/"
rm -f "$nogh/gh"                                                 # the one fake this PATH must not carry
for t in bash jq awk cat tr sed head tail paste sort diff grep wc cut sleep mkdir chmod date seq env; do
  [ -e "$nogh/$t" ] || ln -s "$(command -v "$t")" "$nogh/$t"
done
echo ST1-1 >"$state_dir/records/root-issues"
issue_fixture ST1-1 retro >"$FIX/issue-ST1-1.json"
expect_blocked "done" 'gh is not on PATH: needed to confirm the pull requests merged' PATH="$nogh"
refute grep -Fq 'api/v1/issues' "$FAKE_LOG"                      # blocked before any Dispatch read
touch "$FIX/gh-fails"
expect_blocked "done" 'gh cannot list pull requests on sjawhar/legion-smoke (unauthenticated, rate-limited, or offline)'
rm -f "$FIX/gh-fails"
# with gh answering, the issue not yet done is a retry and the FAILED names the issue
expect_failed "done" "ST1-1 is 'retro', not done"
echo "checkpoints.test.sh: done OK"

# ---- host-daemon checkpoints --------------------------------------------------------------------
host_records() {
  echo host >"$state_dir/records/daemon-mode"
  echo legion-demo >"$state_dir/records/controller-tmux-server"
  echo legion-smoke-t1-worker >"$state_dir/records/legion-node"
  echo legion-demo-providers >"$state_dir/records/providers-secret"
  mkdir -p "$state_dir/host-daemon"
}

# exec-auth accepts its initial mint and its explicit refresh form, but rejects unrecognised flags.
plant_records
reset_fixtures
host_records
printf '2026-09-17T00:00:00Z\n' >"$state_dir/host-daemon/exec-calls.log"
printf 'kubeconfig exec plugin minted a token\n' >"$state_dir/logs/daemon.log"
expect_ok exec-auth 'minted 1 token'
(sleep 1; printf '2026-09-17T00:01:00Z\n' >>"$state_dir/host-daemon/exec-calls.log") &
expect_ok exec-auth 'refreshed from 1 to 2 token mints' --wait-refresh SMOKE_POLL_INTERVAL=1 SMOKE_WAIT_EXEC_AUTH=3
status=0
run_cp exec-auth --unknown-flag || status=$?
[ "$status" = 2 ]
grep -Fq 'unknown checkpoint flag --unknown-flag' "$tmp/out.txt"

# controller-pane cross-checks the daemon's tmux locator against a live pane on the recorded server.
plant_records
reset_fixtures
host_records
base_state | jq '.controllerLocator = {runtime:"tmux",tmuxSession:"legion-demo",tmuxWindowId:"@0",tmuxPaneId:"%0"}' >"$FIX/state.json"
printf 'omp\n' >"$FIX/tmux-panes"
expect_ok controller-pane 'controller pane running on tmux server legion-demo'
assert_file "$state_dir/records/controller-pane-capture"
assert_eq "$(<"$state_dir/records/controller-pane-capture")" "legion-demo @0 $state_dir/logs/controller-pane.log"
grep -Fq "pipe-pane -t @0 -o cat >>$state_dir/logs/controller-pane.log" "$FAKE_LOG"
: >"$FIX/tmux-panes"
expect_failed controller-pane 'daemon state has a tmux controller locator but server legion-demo has no pane'

# scheduling rejects a valid-looking Legion pod that loses any placement or hardening invariant.
plant_records
reset_fixtures
host_records
base_state >"$FIX/state.json"
scheduled="$(pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running |
  jq '.metadata.annotations = {"karpenter.sh/do-not-disrupt":"true"} |
      .spec.nodeName = "legion-smoke-t1-worker" |
      .spec.priorityClassName = "legion" |
      .spec.tolerations = [{key:"legion.dev/pool",operator:"Equal",value:"legion",effect:"NoSchedule"}] |
      .spec.initContainers |= map(.securityContext = {allowPrivilegeEscalation:false,capabilities:{drop:["ALL"]}}) |
      .spec.containers |= map(.securityContext = {allowPrivilegeEscalation:false,capabilities:{drop:["ALL"]}})')"
printf '{"items":[%s]}\n' "$scheduled" >"$FIX/pods.json"
printf '{"data":{"ANTHROPIC_API_KEY":"YW50aHJvcGljLWNhbmFyeS12YWx1ZQ==","DISPATCH_TOKEN":"ZGlzcGF0Y2gtc2VjcmV0LXZhbHVlLTAxMjM0NTY3ODk=","ENVOY_TOKEN":"ZW52b3ktc2VjcmV0LXZhbHVlLTAxMjM0NTY3ODk="}}\n' >"$FIX/secret-legion-demo-providers.json"
expect_ok scheduling '1 Legion pod(s) satisfy placement and hardening'
printf '{"items":[%s]}\n' "$(printf '%s' "$scheduled" | jq '.spec.nodeName = "wrong-node"')" >"$FIX/pods.json"
expect_failed scheduling 'pod legion-st1-1-architect-g1 spec.nodeName is wrong-node, expected legion-smoke-t1-worker'

# plugin-skew installs a newer tarball, restarts the host daemon, and requires one warning per live process.
plant_records
reset_fixtures
host_records
base_state | jq '(.trees["ST1-1"].locator.pluginVersion, .roles["legion-demo-st1-1-architect"].locator.pluginVersion) = "1.0.0"' >"$FIX/state.json"
printf '[legion] live process ST1-1 architect (pod legion-st1-1-architect-g1) runs pi-legion-envoy 1.0.0; installed 2.0.0 — relaunch it (LEGION-164)\n' >"$state_dir/logs/daemon.log"
mkdir -p "$tmp/plugin/package"
printf '{"name":"@sjawhar/pi-legion-envoy","version":"2.0.0"}\n' >"$tmp/plugin/package/package.json"
tar -czf "$tmp/pi-legion-envoy-2.0.0.tgz" -C "$tmp/plugin" package
expect_ok plugin-skew '1 live process warning' SMOKE_PLUGIN_TGZ="$tmp/pi-legion-envoy-2.0.0.tgz" SMOKE_DAEMON_CTL=daemon-ctl.sh
assert_grep 'omp plugin install .*/host-daemon/plugin-skew' "$FAKE_LOG"
grep -Fxq stop "$FIX/daemon-ctl-calls"
grep -Fxq start "$FIX/daemon-ctl-calls"
plant_records
reset_fixtures
host_records
base_state | jq '(.trees["ST1-1"].locator.pluginVersion, .roles["legion-demo-st1-1-architect"].locator.pluginVersion) = "1.0.0" | .controllerLocator = {runtime:"tmux",tmuxSession:"legion-demo",tmuxWindowId:"@0",tmuxPaneId:"%0",pluginVersion:"1.0.0"}' >"$FIX/state.json"
printf '[legion] live process ST1-1 architect (pod legion-st1-1-architect-g1) runs pi-legion-envoy 1.0.0; installed 2.0.0 — relaunch it (LEGION-164)\n[legion] live process controller controller (pane %%0) runs pi-legion-envoy 1.0.0; installed 2.0.0 — relaunch it (LEGION-164)\n' >"$state_dir/logs/daemon.log"
expect_ok plugin-skew '2 live process warnings' SMOKE_PLUGIN_TGZ="$tmp/pi-legion-envoy-2.0.0.tgz" SMOKE_DAEMON_CTL=daemon-ctl.sh
plant_records
reset_fixtures
expect_blocked plugin-skew 'plugin-skew needs SMOKE_DAEMON_MODE=host'

# volume-lost deletes a live worker's pod and PVC, then requires an explicit recovered workspace.
plant_records
reset_fixtures
host_records
echo ok >"$state_dir/records/checkpoint-kill-pod-resume"
base_state | jq '.roles["legion-demo-st1-1-planner"] = {role:"planner",issue:"ST1-1",generation:1,sessionId:"planner-old",readyConfirmedAt:"2026-09-15T00:00:00Z",locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-1-planner-g1",podUid:"u2",pvcName:"legion-st1-1"}}' >"$FIX/state-1.json"
base_state |
  jq '(.trees["ST1-1"].generation, .roles["legion-demo-st1-1-architect"].generation, .roles["legion-demo-st1-1-planner"].generation) = 2 |
      .trees["ST1-1"].locator.podName = "legion-st1-1-architect-g2" |
      .trees["ST1-1"].workspaceLost = {fromRef:"legion/ST1-1"} |
      .roles["legion-demo-st1-1-architect"].locator.podName = "legion-st1-1-architect-g2" |
      .roles["legion-demo-st1-1-architect"].sessionId = "architect-new" |
      .roles["legion-demo-st1-1-architect"].workspaceLost = {fromRef:"legion/ST1-1"} |
      .roles["legion-demo-st1-1-planner"] = {role:"planner",issue:"ST1-1",generation:2,sessionId:"planner-new",readyConfirmedAt:"2026-09-15T00:00:00Z",launchFailures:0,workspaceLost:{fromRef:"legion/ST1-1"},locator:{runtime:"kubernetes",namespace:"legion",podName:"legion-st1-1-planner-g2",podUid:"u3",pvcName:"legion-st1-1"}}' >"$FIX/state-2.json"
printf 'state-1.json\nstate-2.json\n' >"$FIX/state.seq"
pod_fixture legion-st1-1-architect-g1 architect ST1-1 1 Running >"$FIX/pod-legion-st1-1-architect-g1.json"
pod_fixture legion-st1-1-planner-g1 planner ST1-1 1 Running >"$FIX/pod-legion-st1-1-planner-g1.json"
pod_fixture legion-st1-1-architect-g2 architect ST1-1 2 Running |
  jq '.spec.containers[0].command[-1] = "Your workspace was recreated from legion/ST1-1"' >"$FIX/pod-legion-st1-1-architect-g2.json"
pod_fixture legion-st1-1-planner-g2 planner ST1-1 2 Running |
  jq '.spec.containers[0].command[-1] = "Your workspace was recreated from legion/ST1-1"' >"$FIX/pod-legion-st1-1-planner-g2.json"
printf 'worker-recovered\n' >"$state_dir/logs/daemon.log"
expect_ok volume-lost 'tree ST1-1 recovered root legion-st1-1-architect-g2 and every recorded worker from volume loss'
grep -Fxq deleted-tree-pods "$FIX/deleted"
grep -Fxq deleted-pvc-legion-st1-1 "$FIX/deleted"


# The destructive volume-loss step must not select a merely Running, unregistered worker.
plant_records
reset_fixtures
host_records
echo ok >"$state_dir/records/checkpoint-kill-pod-resume"
base_state | jq 'del(.roles["legion-demo-st1-1-architect"].sessionId, .roles["legion-demo-st1-1-architect"].readyConfirmedAt)' >"$FIX/state.json"
expect_failed volume-lost 'root architect of ST1-1 is not ready-confirmed' SMOKE_WAIT_VOLUME_LOST=1
refute test -e "$FIX/deleted"
echo "checkpoints.test.sh: host checkpoints OK"
echo "checkpoints.test.sh: OK"
