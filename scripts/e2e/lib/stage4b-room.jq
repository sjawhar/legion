# Stage 4b's capacity reads of the legion NodePool (stage4b-sandbox-tree.sh's preflight and
# issue-independence): the reservation an issue pod carries, how many of the run's pods the pool can
# place now, and whether an instance type the pool allows can hold two of them. No pod asks for or
# keeps off another pod's node (LEGION-632), so room is counted in pods of the run's largest
# reservation, never in nodes per tree. Loaded with `jq -L scripts/e2e/lib` and
# `include "stage4b-room";`; quantity and roles come from stage4b-pods.jq.
include "stage4b-pods";

# issue_pod_reservation($expected) is an issue pod's reservation, cores and bytes: the sum of its
# six role containers' cpu and memory (EXPECTED maps each role to its {cpu, memory}). The init
# containers take one role's reservation and add nothing: a pod's effective request is the larger of
# its largest init container and its containers' sum (the Kubernetes pod-resources rule), and no
# one role's reservation exceeds the sum of the six.
def issue_pod_reservation($expected):
  {cpu: ([roles[] | $expected[.].cpu | quantity] | add),
   memory: ([roles[] | $expected[.].memory | quantity] | add)};

# pod_request is a pod's effective request, cores and bytes, as the scheduler counts it against a
# node: the larger of its containers' summed requests and its largest init container's, a container
# that requests nothing counting as 0.
def pod_request:
  [.spec.containers[]? | .resources.requests // {}] as $c
  | [.spec.initContainers[]? | .resources.requests // {}] as $i
  | def total($list; $key): [$list[] | .[$key] // 0 | quantity] | add // 0;
    def largest($list; $key): [$list[] | .[$key] // 0 | quantity] | max // 0;
    {cpu: ([total($c; "cpu"), largest($i; "cpu")] | max),
     memory: ([total($c; "memory"), largest($i; "memory")] | max)};

# placeable_node keeps a pool node the scheduler can place a pod on: Ready, schedulable, not tainted
# karpenter.sh/disrupted and not being deleted.
def placeable_node:
  select((.spec.unschedulable // false) | not)
  | select(.metadata.deletionTimestamp == null)
  | select(any(.spec.taints[]?; .key == "karpenter.sh/disrupted") | not)
  | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"));

# fits($free; $pod) is how many pods of reservation POD fit in FREE (both {cpu, memory}), never
# below 0.
def fits($free; $pod):
  [($free.cpu / $pod.cpu), ($free.memory / $pod.memory)] | min | floor | if . < 0 then 0 else . end;

# pool_room($pool; $nodes; $pods; $pod) is how many pods of reservation POD the legion pool can place
# now, from the NodePool POOL, its NODES (the `karpenter.sh/nodepool=legion` list) and every pod of
# the cluster PODS (a pod list across namespaces): `pod`, the reservation; `free_nodes`, each
# placeable node with what its allocatable has free beside every non-terminal pod placed on it and
# how many such pods fit there; `new_pods`, the pods the pool's cpu and memory limits leave room for
# beside what it already runs (`status.resources`), null when the pool sets no limit; and `room`,
# their sum, null when `new_pods` is null, since a pool without limits bounds nothing.
def pool_room($pool; $nodes; $pods; $pod):
  ([$pods.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed") | select(.spec.nodeName != null)]) as $placed
  | [$nodes.items[] | placeable_node
      | .metadata.name as $name
      | ([$placed[] | select(.spec.nodeName == $name) | pod_request]) as $requests
      | {node: $name,
         free: {cpu: ((.status.allocatable.cpu | quantity) - ([$requests[].cpu] | add // 0)),
                memory: ((.status.allocatable.memory | quantity) - ([$requests[].memory] | add // 0))}}
      | .fits = fits(.free; $pod)] as $free_nodes
  | ($pool.spec.limits // {}) as $limit | ($pool.status.resources // {}) as $used
  | [ if $limit.cpu then ((($limit.cpu | quantity) - (($used.cpu // 0) | quantity)) / $pod.cpu | floor) else empty end,
      if $limit.memory then ((($limit.memory | quantity) - (($used.memory // 0) | quantity)) / $pod.memory | floor) else empty end
    ] as $by_limit
  | {pod: $pod, free_nodes: $free_nodes,
     new_pods: (if ($by_limit | length) == 0 then null else ([$by_limit | min, 0] | max) end)}
  | .room = (if .new_pods == null then null else ([.free_nodes[].fits] | add // 0) + .new_pods end);

# instance_bound($key) is the largest value of the Karpenter instance requirement KEY the NodePool on
# input allows (its `Lt` values less one, and the largest of an `In` list; the smallest of those when
# both are set), or null when the requirements bound no maximum.
def instance_bound($key):
  [.spec.template.spec.requirements[]? | select(.key == $key)
    | if .operator == "Lt" then (.values[0] | tonumber) - 1
      elif .operator == "In" then ([.values[] | tonumber] | max)
      else empty end]
  | if length == 0 then null else min end;

# two_pods_per_instance($pod) decides, from the NodePool on input alone, whether an instance type it
# allows can hold two pods of reservation POD: `fit` is false when its instance-cpu or
# instance-memory requirements (vCPUs; MiB) bound every allowed instance below two pods' worth, so
# two of the run's pods can share no node and must land on two; true when both bounds hold two or
# more; null when the requirements bound no maximum in one of the two, so the read decides nothing.
# `reason` says which, with the numbers. The bound is the instance's size, above a node's
# allocatable, so false is never wrong while true says only that the requirements allow it.
def two_pods_per_instance($pod):
  instance_bound("karpenter.k8s.aws/instance-cpu") as $cpu
  | instance_bound("karpenter.k8s.aws/instance-memory") as $memory
  | ($pod.cpu * 2) as $need_cpu | ($pod.memory * 2 / 1048576) as $need_memory
  | if $cpu != null and $cpu < $need_cpu then
      {fit: false, reason: "the NodePool allows at most \($cpu) vCPUs an instance, under the \($need_cpu) two pods reserve, so no node holds two of the run's pods"}
    elif $memory != null and $memory < $need_memory then
      {fit: false, reason: "the NodePool allows at most \($memory) MiB an instance, under the \($need_memory) two pods reserve, so no node holds two of the run's pods"}
    elif $cpu != null and $memory != null then
      {fit: true, reason: "the NodePool allows instances of up to \($cpu) vCPUs and \($memory) MiB, room for two pods (\($need_cpu) vCPUs, \($need_memory) MiB) on paper, so the pods may share a node"}
    else
      {fit: null, reason: "the NodePool bounds no maximum instance-\(if $cpu == null then "cpu" else "memory" end), so whether two of the run's pods (\($need_cpu) vCPUs, \($need_memory) MiB) can share a node cannot be read from it"}
    end;
