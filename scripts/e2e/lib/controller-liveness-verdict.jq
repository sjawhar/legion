# Stage 4b's verdict on the controller liveness lines (stage4b-sandbox-tree.sh's
# daemon-controller-liveness): the two lines a log monitor on the daemon counts while its project has
# no live controller, read from the part of the daemon's JSON log after the driver's last action.
# Input: that part of the log, raw (jq -R -s); a line that is not a JSON object is skipped.
# Arguments, by name ($ARGS.named):
#   phase     down: the controller the daemon launched (claim `claim`) is gone and cannot come back.
#             The record still names its session `session`, the driver deleted its pod (incarnation
#             `deleted`), and every relaunch's pod stays unschedulable until the runtime retires it.
#             up: the controller the daemon launched is registered and holds the controller role.
#             operator: `controller: operator`, with no controller registered.
#   boot      the worker boot timeout in seconds (--argjson), the least time between two
#             not-registered lines.
# Output: {notRegistered, noHolder, deaths, missing, wrong}: the lines of each kind; each death of a
# relaunch of `claim` (any incarnation but `deleted`); what the phase has not seen yet; and each line
# that departs from the phase. Every line of either kind names the phase's mode: daemon for down and
# up, operator for operator. By phase:
#   down      the no-holder line, naming `session`, on at least 3 sweeps (the Prober logs it at
#             every one); the not-registered line at least twice, each at least `boot` after the one
#             before, in the daemon's own form (the monitored text leading the remedy that names
#             controller: operator), saying the record is registered and naming a claim state that
#             is not a live controller's; a relaunch that died; and a not-registered line after it.
#   up        neither line.
#   operator  the not-registered line, exactly the monitored text.
def not_registered: "controller not registered; run legion controller start";
def no_holder: "controller liveness: the controller role has no live holder; the controller is gone";
# secs is an RFC 3339 time as seconds since the epoch, its fraction and offset included.
def secs:
  (capture("^(?<s>[0-9-]+T[0-9:]+)(\\.(?<f>[0-9]+))?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
    // error("not an RFC 3339 time: \(.)")) as $t
  | (($t.s + "Z") | fromdateiso8601) + (($t.f // "0") | "0." + . | tonumber)
    - (if $t.z == "Z" then 0
       else (($t.z[1:3] | tonumber) * 3600 + ($t.z[4:6] | tonumber) * 60) * (if $t.z[0:1] == "-" then -1 else 1 end) end);
def live_state: IN("ready", "working", "idle");
($ARGS.named.phase // "") as $phase
| if ($phase | IN("down", "up", "operator") | not) then error("phase must be down, up or operator, not \($phase | tojson)") else . end
| ($ARGS.named.boot // error("boot (the worker boot timeout in seconds) is required")) as $boot
| ($ARGS.named.claim // "") as $claim
| ($ARGS.named.session // "") as $session
| ($ARGS.named.deleted // "") as $deleted
| (if $phase == "operator" then "operator" else "daemon" end) as $mode
| [split("\n")[] | fromjson? | select(type == "object" and (.msg | type) == "string")] as $log
| [range($log | length) as $i | $log[$i] + {i: $i}] as $log
| [$log[] | select(.msg | startswith(not_registered))] as $nr
| [$log[] | select(.msg == no_holder)] as $nh
| [$log[] | select(.msg == "supervise: process died" and .claim == $claim and .incarnation != $deleted)] as $deaths
| ([$nr[], $nh[] | select(.mode != $mode) | "\(.time) \(.msg | tojson) names mode \(.mode // "none" | tojson), not \($mode)"]) as $modes
| (if $phase == "down" then
    ([$nr[] | .time as $at
      | (if .msg == not_registered or (.msg | contains("controller: operator") | not)
           then "\($at) \(.msg | tojson) is not the daemon's form: the monitored text leading the remedy that names controller: operator" else empty end),
        (if .registered != true then "\($at) the not-registered line says the record is registered: \(.registered | tojson), want true (it still names \($session))" else empty end),
        (if (.claimState | type) != "string" or (.claimState | live_state)
           then "\($at) the not-registered line names claim state \(.claimState | tojson), not a claim that is down" else empty end)]
     + [$nh[] | select(.session != $session) | "\(.time) the no-holder line names session \(.session | tojson), not \($session)"]
     + ([$nr[] | .time | secs] as $t
        | [range(1; $t | length) | select($t[.] - $t[. - 1] < $boot)
           | "\($nr[.].time) the not-registered line came \(($t[.] - $t[. - 1]) * 1000 | round / 1000) s after the one before, under the boot timeout of \($boot) s"]))
  elif $phase == "up" then
    [$nr[], $nh[] | "\(.time) \(.msg | tojson) while the daemon's controller holds the role"]
  else
    [$nr[] | select(.msg != not_registered) | "\(.time) \(.msg | tojson) is not exactly the monitored text"]
  end) as $departures
| (if $phase == "down" then
    (if ($nh | length) < 3 then ["the no-holder line on 3 sweeps (seen \($nh | length))"] else [] end)
    + (if ($nr | length) < 2 then ["the not-registered line twice (seen \($nr | length))"] else [] end)
    + (if ($deaths | length) == 0 then ["a relaunch of \($claim) that died"]
       elif ([$nr[] | select(.i > $deaths[0].i)] | length) == 0 then ["a not-registered line after the relaunch that died at \($deaths[0].time)"]
       else [] end)
  elif $phase == "operator" then
    (if ($nr | length) == 0 then ["the operator's not-registered line"] else [] end)
  else [] end) as $missing
| {
    notRegistered: [$nr[] | {time, msg, claimState, registered}],
    noHolder: [$nh[] | {time, session}],
    deaths: [$deaths[] | {time, incarnation, observed}],
    missing: $missing,
    wrong: ($modes + $departures)
  }
