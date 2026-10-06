# Stage 4b's reads of the pods its pod watch recorded (stage4b-sandbox-tree.sh): one definition of
# a Sandbox pod the run judges, of the address its worker shim dials, and of the worker stream the
# daemon served when the pod was created. Loaded with `jq -L scripts/e2e/lib` and
# `include "stage4b-pods";`.

# ready_pod_event keeps a pod watch event whose pod is a Sandbox pod of the run (the image probe and
# the run's controls aside) with its worker container ready.
def ready_pod_event:
  select(.object.kind == "Pod"
    and .object.metadata.labels["legion.dev/probe"] == null
    and .object.metadata.labels["legion.dev/e2e-control"] == null
    and any(.object.status.containerStatuses[]?; .name == "worker" and .ready));

# ready_pods is each such event's pod.
def ready_pods: ready_pod_event | .object;

# shim_connect is the address a pod's worker shim dials: the argument after `--connect` in its
# worker container's command, or null with none.
def shim_connect:
  [.spec.containers[]? | select(.name == "worker") | (.command // []) as $c
    | range($c | length) | select($c[.] == "--connect") | $c[. + 1]] | first;

# served_stream(streams) is the worker stream the daemon served when the pod was created: the
# stream of the last of STREAMS (record_stream's {since, stream} records, one per daemon start, in
# order) that started at or before the pod's creationTimestamp, or null with none. Both times are
# whole-second UTC RFC 3339 strings, which order as text.
def served_stream($streams):
  .metadata.creationTimestamp as $created
  | [$streams[] | select(.since <= $created)] | last | .stream;
