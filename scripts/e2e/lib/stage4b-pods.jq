# Stage 4b's reads of the pods its pod watch recorded (stage4b-sandbox-tree.sh): one definition of
# a Sandbox pod the run judges, of whether the scheduler placed a pod, of the address its launcher
# dials, of the worker stream the daemon served when the pod was created, and of the reservation
# every container carries. Loaded with `jq -L scripts/e2e/lib` and `include "stage4b-pods";`.

# ready_pod_event keeps a pod watch event whose pod is a Sandbox issue pod of the run (the image
# probe and the run's controls aside) whose one container, `roles`, is ready.
def ready_pod_event:
  select(.object.kind == "Pod"
    and .object.metadata.labels["legion.dev/probe"] == null
    and .object.metadata.labels["legion.dev/e2e-control"] == null)
  | select(any(.object.status.containerStatuses[]?; .name == "roles" and .ready));

# ready_pods is each such event's pod.
def ready_pods: ready_pod_event | .object;

# unschedulable is whether the scheduler reported it could not place a pod: PodScheduled=False with
# reason Unschedulable.
def unschedulable:
  any(.status.conditions[]?; .type == "PodScheduled" and .status == "False" and .reason == "Unschedulable");

# scheduled is whether the scheduler placed a pod: bound to a node, or PodScheduled=True.
def scheduled:
  (.spec.nodeName // "") != "" or any(.status.conditions[]?; .type == "PodScheduled" and .status == "True");

# launcher_connect is the address the launcher of an issue pod's `roles` container dials, the
# argument after `--connect` in its command, or null with no such container or no `--connect`.
def launcher_connect:
  [.spec.containers[]? | select(.name == "roles") | (.command // []) as $c
    | [range($c | length) | select($c[.] == "--connect") | $c[. + 1]] | first] | first;

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

# reservation_problems($expected) prints each way an issue pod departs from the run's reservation,
# or nothing. EXPECTED maps each pod the daemon builds, `issue` and `controller`, to its {cpu,
# memory, ephemeral_storage, ephemeral_storage_request}, the run's overrides and defaults settled as
# the daemon settles them (run_resources). Every container of an issue pod, the init containers
# included, carries cpu and memory as both request and limit and ephemeral-storage as a request no
# greater than its limit (the node-disk bound between pods of unrelated trees, which QoS does not
# read), and all four values are the issue pod's one reservation (issuePod.resources,
# internal/runtime/sandbox/podkind.go); the pod is Guaranteed; and it carries no affinity, since a
# Legion pod asks nothing of its placement beyond the pool.
def reservation_problems($expected):
  .spec as $s | $expected.issue as $e
  | def same($a; $b): ($a | quantity) == ($b | quantity);
    def carried($req; $lim): "cpu \($req.cpu), memory \($req.memory), ephemeral-storage \($req["ephemeral-storage"]) of \($lim["ephemeral-storage"])";
    def expected_of($e): "cpu \($e.cpu), memory \($e.memory), ephemeral-storage \($e.ephemeral_storage_request) of \($e.ephemeral_storage)";
    def matches($e; $req; $lim):
      same($req.cpu; $e.cpu) and same($req.memory; $e.memory)
      and same($req["ephemeral-storage"]; $e.ephemeral_storage_request) and same($lim["ephemeral-storage"]; $e.ephemeral_storage);
    ([$s.initContainers[]?, $s.containers[]?] | .[] as $c
      | ($c.resources.requests // {}) as $req | ($c.resources.limits // {}) as $lim
      | if $req.cpu == null or $req.memory == null or $lim.cpu == null or $lim.memory == null
           or $req["ephemeral-storage"] == null or $lim["ephemeral-storage"] == null
        then "container \($c.name) reserves \($c.resources // {} | tojson), want cpu and memory as both request and limit and ephemeral-storage as request and limit"
        elif (same($req.cpu; $lim.cpu) and same($req.memory; $lim.memory)) | not
        then "container \($c.name) requests \($req | tojson) but is limited to \($lim | tojson); this run configures request = limit"
        elif ($req["ephemeral-storage"] | quantity) > ($lim["ephemeral-storage"] | quantity)
        then "container \($c.name) requests ephemeral-storage \($req["ephemeral-storage"]) past its limit \($lim["ephemeral-storage"]); the request may not exceed the limit"
        elif matches($e; $req; $lim) | not
        then "container \($c.name) reserves \(carried($req; $lim)), not the run's \(expected_of($e)) for the issue pod"
        else empty end),
    (if .status.qosClass != "Guaranteed" then "qosClass \(.status.qosClass // "unset"), want Guaranteed" else empty end),
    (if $s.affinity != null then "affinity \($s.affinity | tojson): a Legion pod asks nothing of its placement beyond the pool" else empty end);
