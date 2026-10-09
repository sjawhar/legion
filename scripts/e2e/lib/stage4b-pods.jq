# Stage 4b's reads of the pods its pod watch recorded (stage4b-sandbox-tree.sh): one definition of
# a Sandbox pod the run judges, of the address its role launchers dial, and of the worker stream the
# daemon served when the pod was created. Loaded with `jq -L scripts/e2e/lib` and
# `include "stage4b-pods";`.

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
