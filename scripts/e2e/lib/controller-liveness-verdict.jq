# Stage 4b's verdict on the controller liveness lines (stage4b-sandbox-tree.sh's
# daemon-controller-liveness): the two lines a log monitor on the daemon counts while its project has
# no live controller, read from the part of the daemon's JSON log after the driver's last action.
# Input: that part of the log, raw (jq -R -s); a line that is not a JSON object is skipped. Run with
# `jq -L scripts/e2e/lib`: whether the scheduler placed a pod is stage4b-pods.jq's reading.
# Arguments, by name ($ARGS.named):
#   phase        down: the controller the daemon launched (claim `claim`) is gone and cannot come
#                back. The record still names its session `session`, the driver deleted its pod (uid
#                `deleted_pod`), and each relaunch's pod requests `cpu` CPU, more than any node holds.
#                up: the controller the daemon launched is registered and holds the controller role.
#                operator: `controller: operator`, with no controller registered.
#   boot         the worker boot timeout in seconds (--argjson), the least time between two
#                not-registered lines.
#   watch        down: the run's pod watch, one event a line (--slurpfile).
# Output: {notRegistered, noHolder, launchFailures, launches, pods, missing, wrong}: the lines of each
# kind; in the down phase, the claim's failed launches and its launches, and the pods a relaunch
# made, those of the controller whose containers request `cpu`; what the phase has not seen yet; and
# each line or pod that departs from the phase. Every line of either kind names the phase's mode:
# daemon for down and up, operator for operator. By phase, one definition each below:
#   down         the no-holder line, naming `session`, on at least 3 sweeps (the Prober logs it at
#                every one); the not-registered line at least twice, each at least `boot` after the
#                one before, in the daemon's own form (the monitored text leading the remedy that
#                names controller: operator), saying the record is registered and naming a claim
#                state that is not a live controller's; a failed launch of `claim` (a launch onto a
#                pod that never schedules waits for its role launcher until the boot timeout and
#                fails, and the machine records no process for it), and a not-registered line after
#                the first; a pod a relaunch made, every one of them seen unschedulable and never
#                scheduled (stage4b-pods.jq). A launch of `claim` is only ever on the deleted pod: a
#                relaunch can catch that pod while it terminates and return it, and it then dies.
#                The pod the Agent Sandbox controller recreates from the old template, and the
#                deleted pod, request the earlier CPU, so they are not a relaunch's.
#   up           neither line.
#   operator     the not-registered line, exactly the monitored text.
include "stage4b-pods";
def not_registered: "controller not registered; run legion controller start";
def no_holder: "controller liveness: the controller role has no live holder; the controller is gone";
# secs is an RFC 3339 time as seconds since the epoch, its fraction and offset included.
def secs:
  (capture("^(?<s>[0-9-]+T[0-9:]+)(\\.(?<f>[0-9]+))?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
    // error("not an RFC 3339 time: \(.)")) as $t
  | (($t.s + "Z") | fromdateiso8601) + (($t.f // "0") | "0." + . | tonumber)
    - (if $t.z == "Z" then 0
       else (($t.z[1:3] | tonumber) * 3600 + ($t.z[4:6] | tonumber) * 60) * (if $t.z[0:1] == "-" then -1 else 1 end) end);
# millicores is a Kubernetes CPU quantity in millicores: the API server writes one in its canonical
# decimal form ("100k" for 100000).
def millicores:
  (capture("^(?<n>[0-9]+(\\.[0-9]+)?)(?<s>[mkMGTPE])?$") // error("not a CPU quantity: \(.)")) as $q
  | ($q.n | tonumber) * {"m": 1, "": 1000, "k": 1e6, "M": 1e9, "G": 1e12, "T": 1e15, "P": 1e18, "E": 1e21}[$q.s // ""];
def live_state: IN("ready", "working", "idle");
($ARGS.named.phase // "") as $phase
| if ($phase | IN("down", "up", "operator") | not) then error("phase must be down, up or operator, not \($phase | tojson)") else . end
| ($ARGS.named.boot // error("boot (the worker boot timeout in seconds) is required")) as $boot
| (if $phase == "operator" then "operator" else "daemon" end) as $mode
| [split("\n")[] | fromjson? | select(type == "object" and (.msg | type) == "string")] as $log
| [range($log | length) as $i | $log[$i] + {i: $i}] as $log
| [$log[] | select(.msg | startswith(not_registered))] as $nr
| [$log[] | select(.msg == no_holder)] as $nh
# Each phase is the part of the output it judges: {launchFailures, launches, pods, missing, wrong}.
| def down:
    ($ARGS.named.claim // "") as $claim
    | ($ARGS.named.session // "") as $session
    | ($ARGS.named.deleted_pod // "") as $deleted_pod
    | (($ARGS.named.cpu // error("cpu (each relaunch's CPU request) is required in the down phase")) | millicores) as $cpu
    | [$log[] | select(.msg == "supervise: launch failed" and .claim == $claim)] as $failed
    | [$log[] | select(.msg == "supervise: launched" and .claim == $claim)] as $launched
    | ([($ARGS.named.watch // error("watch (the pod watch, --slurpfile) is required in the down phase"))[]
          | select(.object.kind == "Pod" and .object.metadata.labels["legion.dev/role"] == "controller") | .object
          | select(any(.spec.containers[]?; (.resources.requests.cpu // "0" | millicores) == $cpu))]
        | group_by(.metadata.uid)
        | map({uid: .[0].metadata.uid, name: .[0].metadata.name, unschedulable: any(.[]; unschedulable),
            node: ([.[] | .spec.nodeName // empty] | first), scheduled: any(.[]; scheduled)})) as $pods
    | {
        launchFailures: [$failed[] | {time, generation, launchFailures}],
        launches: [$launched[] | {time, incarnation}],
        pods: $pods,
        missing: ((if ($nh | length) < 3 then ["the no-holder line on 3 sweeps (seen \($nh | length))"] else [] end)
          + (if ($nr | length) < 2 then ["the not-registered line twice (seen \($nr | length))"] else [] end)
          + (if ($failed | length) == 0 then ["a failed launch of \($claim)"]
             elif ([$nr[] | select(.i > $failed[0].i)] | length) == 0 then ["a not-registered line after the failed launch at \($failed[0].time)"]
             else [] end)
          + (if ($pods | length) == 0 then ["a pod a relaunch made, requesting \($ARGS.named.cpu) CPU"]
             else [$pods[] | select((.unschedulable or .scheduled) | not) | "pod \(.name) (uid \(.uid)) seen Unschedulable"] end)),
        wrong: ([$nr[] | .time as $at
            | (if .msg == not_registered or (.msg | contains("controller: operator") | not)
                 then "\($at) \(.msg | tojson) is not the daemon's form: the monitored text leading the remedy that names controller: operator" else empty end),
              (if .registered != true then "\($at) the not-registered line says the record is registered: \(.registered | tojson), want true (it still names \($session))" else empty end),
              (if (.claimState | type) != "string" or (.claimState | live_state)
                 then "\($at) the not-registered line names claim state \(.claimState | tojson), not a claim that is down" else empty end)]
          + [$nh[] | select(.session != $session) | "\(.time) the no-holder line names session \(.session | tojson), not \($session)"]
          + ([$nr[] | .time | secs] as $t
             | [range(1; $t | length) | select($t[.] - $t[. - 1] < $boot)
                | "\($nr[.].time) the not-registered line came \(($t[.] - $t[. - 1]) * 1000 | round / 1000) s after the one before, under the boot timeout of \($boot) s"])
          + [$launched[] | select((.incarnation // "" | split("/")[0]) != $deleted_pod)
             | "\(.time) \($claim) launched at \(.incarnation | tojson), not on the deleted pod \($deleted_pod): its controller came back"]
          + [$pods[] | select(.scheduled)
             | "pod \(.name) (uid \(.uid)), made by a relaunch requesting \($ARGS.named.cpu) CPU, was scheduled\(if .node then " on \(.node)" else "" end)"])
      };
  def up: {wrong: [$nr[], $nh[] | "\(.time) \(.msg | tojson) while the daemon's controller holds the role"]};
  def operator:
    {missing: (if ($nr | length) == 0 then ["the operator's not-registered line"] else [] end),
     wrong: [$nr[] | select(.msg != not_registered) | "\(.time) \(.msg | tojson) is not exactly the monitored text"]};
  {
    notRegistered: [$nr[] | {time, msg, claimState, registered}],
    noHolder: [$nh[] | {time, session}],
    launchFailures: [],
    launches: [],
    pods: [],
    missing: [],
    wrong: []
  }
  + (if $phase == "down" then down elif $phase == "up" then up else operator end)
| .wrong = ([$nr[], $nh[] | select(.mode != $mode) | "\(.time) \(.msg | tojson) names mode \(.mode // "none" | tojson), not \($mode)"] + .wrong)
