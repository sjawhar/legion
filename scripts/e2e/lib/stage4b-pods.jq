# Stage 4b's reads of the pods its pod watch recorded (stage4b-sandbox-tree.sh): one definition of
# a Sandbox pod the run judges, of the address its role launchers dial, of the worker stream the
# daemon served when the pod was created, and of the reservation every container carries. Loaded
# with `jq -L scripts/e2e/lib` and `include "stage4b-pods";`.

# roles are an issue pod's six role containers, one per claim.Roles entry, each named for its role.
def roles: ["architect", "planner", "implementer", "tester", "reviewer", "merger"];

# ready_pod_event keeps a pod watch event whose pod is a Sandbox pod of the run (the image probe and
# the run's controls aside) with all six of its role launchers ready.
def ready_pod_event:
  select(.object.kind == "Pod"
    and .object.metadata.labels["legion.dev/probe"] == null
    and .object.metadata.labels["legion.dev/e2e-control"] == null)
  | ([.object.status.containerStatuses[]? | select(.name as $name | roles | index($name))]) as $status
  | select(($status | map(.name) | sort) == (roles | sort) and all($status[]; .ready));

# ready_pods is each such event's pod.
def ready_pods: ready_pod_event | .object;

# launcher_connects is, for each role container of a pod, its role and the address its launcher
# dials: the argument after `--connect` in its command, or null with none.
def launcher_connects:
  [.spec.containers[]? | select(.name as $name | roles | index($name)) | .name as $role | (.command // []) as $c
    | {role: $role, connect: ([range($c | length) | select($c[.] == "--connect") | $c[. + 1]] | first)}];

# launcher_connect is the one address every role launcher of a pod dials, or null when they name
# none or differ.
def launcher_connect:
  launcher_connects | map(.connect) | unique | if length == 1 then .[0] else null end;

# served_stream(streams) is the worker stream the daemon served when the pod was created: the
# stream of the last of STREAMS (record_stream's {since, stream} records, one per daemon start, in
# order) that started at or before the pod's creationTimestamp, or null with none. Both times are
# whole-second UTC RFC 3339 strings, which order as text, from two clocks: the devbox's dates each
# record and the API server's each pod (record_stream states what comparing them assumes).
def served_stream($streams):
  .metadata.creationTimestamp as $created
  | [$streams[] | select(.since <= $created)] | last | .stream;

# quantity is a Kubernetes quantity as a number: cores for cpu, bytes for memory ("750m" is 0.75,
# "3Gi" is 3221225472, "1" and 1 are 1), as the API server writes them (a decimal with a suffix of
# m, k, M, G, T, Ki, Mi, Gi or Ti, or none). null stays null.
def quantity:
  if . == null then null
  else tostring | capture("^(?<n>[0-9]+(\\.[0-9]+)?)(?<u>m|k|M|G|T|Ki|Mi|Gi|Ti|)$")
    | if .u == "m" then (.n | tonumber) / 1000
      else (.n | tonumber) * {"": 1, k: 1e3, M: 1e6, G: 1e9, T: 1e12, Ki: 1024, Mi: 1048576, Gi: 1073741824, Ti: 1099511627776}[.u] end
  end;

# reservation_problems($expected) prints each way a pod departs from the run's reservations, or
# nothing. EXPECTED maps each role to its {cpu, memory}, the run's overrides and defaults settled as
# the daemon settles them (run_resources). Every container, the init containers included, carries
# cpu and memory as both request and limit; a container named for a role carries that role's
# reservation; an init container, which the pod does not name a role for, carries the reservation
# of one of the pod's roles, the role whose launch created the pod (issuePod.initContainers,
# internal/runtime/sandbox/podkind.go); the pod is Guaranteed; and it carries no affinity, since a
# Legion pod asks nothing of its placement beyond the pool.
def reservation_problems($expected):
  .spec as $s
  | [$s.containers[]? | .name | select($expected[.] != null)] as $pod_roles
  | def same($a; $b): ($a | quantity) == ($b | quantity);
    ([$s.initContainers[]?, $s.containers[]?] | .[] as $c
      | ($c.resources.requests // {}) as $req | ($c.resources.limits // {}) as $lim
      | if $req.cpu == null or $req.memory == null or $lim.cpu == null or $lim.memory == null
        then "container \($c.name) reserves \($c.resources // {} | tojson), want cpu and memory as both request and limit"
        elif (same($req.cpu; $lim.cpu) and same($req.memory; $lim.memory)) | not
        then "container \($c.name) requests \($req | tojson) but is limited to \($lim | tojson); a reservation is one value as both"
        elif $expected[$c.name] != null and ((same($req.cpu; $expected[$c.name].cpu) and same($req.memory; $expected[$c.name].memory)) | not)
        then "the \($c.name) container reserves cpu \($req.cpu), memory \($req.memory), not the run's cpu \($expected[$c.name].cpu), memory \($expected[$c.name].memory) for its role"
        elif $expected[$c.name] == null and ([$pod_roles[] | $expected[.] | select(same(.cpu; $req.cpu) and same(.memory; $req.memory))] | length) == 0
        then "init container \($c.name) reserves cpu \($req.cpu), memory \($req.memory), the reservation of no role of the pod"
        else empty end),
    (if .status.qosClass != "Guaranteed" then "qosClass \(.status.qosClass // "unset"), want Guaranteed" else empty end),
    (if $s.affinity != null then "affinity \($s.affinity | tojson): a Legion pod asks nothing of its placement beyond the pool" else empty end);
