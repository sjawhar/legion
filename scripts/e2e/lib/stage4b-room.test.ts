import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { runJq } from "./run-jq";
import { defaults } from "./stage4b-reservations";

// Stage 4b's capacity reads (lib/stage4b-room.jq): the reservation an issue pod carries under the
// run's overrides and defaults, how many of the run's pods the legion pool can place now, and
// whether an instance type the pool allows can hold two of them, each run on fixtures shaped as
// kubectl prints a NodePool, a node list and a pod list.
const lib = join(import.meta.dir);
const include = 'include "stage4b-room";';
const GiB = 1073741824;
// The run's overrides (stage4b-sandbox-tree.sh's override_cpu and override_memory over the
// defaults): 2.95 CPU and 13 GiB a pod.
const overridden = {
  ...defaults,
  tester: { cpu: "1", memory: "4Gi" },
  reviewer: { cpu: "500m", memory: "2Gi" },
  merger: { cpu: "200m", memory: "1Gi" },
};
const defaultPod = { cpu: 3, memory: 15 * GiB };

function jq(filter: string, input: unknown, args: string[] = []) {
  return JSON.parse(
    runJq(["-c", "-L", lib, ...args, `${include} ${filter}`], JSON.stringify(input))
  );
}

// node is a pool node NAME with ALLOCATABLE, Ready and schedulable unless EDIT says otherwise.
function node(name: string, allocatable: { cpu: string; memory: string }, edit: object = {}) {
  return {
    metadata: { name },
    spec: {},
    status: { allocatable, conditions: [{ type: "Ready", status: "True" }] },
    ...edit,
  };
}
// placed is a pod on NODE in PHASE whose containers request CONTAINERS and whose init containers
// request INIT, each a {cpu, memory} with either side optional.
function placed(
  nodeName: string,
  containers: Record<string, string>[],
  init: Record<string, string>[] = [],
  phase = "Running"
) {
  return {
    spec: {
      nodeName,
      containers: containers.map((requests) => ({ resources: { requests } })),
      initContainers: init.map((requests) => ({ resources: { requests } })),
    },
    status: { phase },
  };
}
// requirement is one Karpenter requirement of a NodePool.
const requirement = (key: string, operator: string, ...values: string[]) => ({
  key,
  operator,
  values,
});
// nodePool is a NodePool with REQUIREMENTS, LIMITS and the resources it already RUNS.
function nodePool(requirements: object[], limits?: object, runs?: object) {
  return {
    spec: { template: { spec: { requirements } }, ...(limits === undefined ? {} : { limits }) },
    status: runs === undefined ? {} : { resources: runs },
  };
}
const cpuFloor = requirement("karpenter.k8s.aws/instance-cpu", "Gt", "3");

describe("issue_pod_reservation", () => {
  test("sums the six role containers' reservations, cores and bytes", () => {
    expect(
      jq("issue_pod_reservation($expected)", null, [
        "--argjson",
        "expected",
        JSON.stringify(defaults),
      ])
    ).toEqual(defaultPod);
  });

  test("takes the run's overrides where it sets them and the defaults elsewhere", () => {
    expect(
      jq("issue_pod_reservation($expected)", null, [
        "--argjson",
        "expected",
        JSON.stringify(overridden),
      ])
    ).toEqual({ cpu: 2.95, memory: 13 * GiB });
  });
});

describe("pod_request", () => {
  test("is the larger of the containers' sum and the largest init container, a missing request 0", () => {
    expect(
      jq(
        "pod_request",
        placed(
          "n",
          [{ cpu: "100m", memory: "128Mi" }, { cpu: "200m" }],
          [{ cpu: "2", memory: "64Mi" }]
        )
      )
    ).toEqual({ cpu: 2, memory: 128 * 1048576 });
    expect(
      jq(
        "pod_request",
        placed(
          "n",
          [
            { cpu: "250m", memory: "1Gi" },
            { cpu: "750m", memory: "3Gi" },
          ],
          [{ cpu: "250m", memory: "1Gi" }]
        )
      )
    ).toEqual({ cpu: 1, memory: 4 * GiB });
    expect(jq("pod_request", { spec: {} })).toEqual({ cpu: 0, memory: 0 });
  });
});

describe("pool_room", () => {
  const nodes = {
    items: [
      // 3.92 CPU and 15 GiB allocatable, a daemonset and an init-heavy pod on it: 1.92 CPU free.
      node("small", { cpu: "3920m", memory: "15Gi" }),
      // Cordoned: never counted.
      node("cordoned", { cpu: "7910m", memory: "30Gi" }, { spec: { unschedulable: true } }),
      // Being disrupted by Karpenter: never counted.
      node(
        "disrupted",
        { cpu: "7910m", memory: "30Gi" },
        {
          spec: { taints: [{ key: "karpenter.sh/disrupted", effect: "NoSchedule" }] },
        }
      ),
      // Not Ready: never counted.
      {
        metadata: { name: "unready" },
        spec: {},
        status: {
          allocatable: { cpu: "7910m", memory: "30Gi" },
          conditions: [{ type: "Ready", status: "False" }],
        },
      },
      // 7.91 CPU and 30 GiB, one issue pod of another project on it and a finished one: room for one more.
      node("large", { cpu: "7910m", memory: "30Gi" }),
    ],
  };
  const pods = {
    items: [
      placed(
        "small",
        [{ cpu: "100m", memory: "128Mi" }, { cpu: "200m" }],
        [{ cpu: "2", memory: "64Mi" }]
      ),
      placed("large", [{ cpu: "3", memory: "15Gi" }]),
      placed("large", [{ cpu: "3", memory: "15Gi" }], [], "Succeeded"),
      placed("cordoned", [{ cpu: "1", memory: "1Gi" }]),
    ],
  };
  const room = (pool: object) =>
    jq("pool_room($pool[0]; $nodes[0]; $pods[0]; $pod)", null, [
      "--argjson",
      "pool",
      JSON.stringify([pool]),
      "--argjson",
      "nodes",
      JSON.stringify([nodes]),
      "--argjson",
      "pods",
      JSON.stringify([pods]),
      "--argjson",
      "pod",
      JSON.stringify(defaultPod),
    ]);

  test("counts the pods that fit the Ready, schedulable, undisrupted nodes' free allocatable, and the pods the limits leave room for", () => {
    const got = room(
      nodePool([cpuFloor], { cpu: "16", memory: "64Gi" }, { cpu: "8", memory: "32Gi" })
    );
    expect(got.pod).toEqual(defaultPod);
    expect(got.free_nodes.map((n: { node: string; fits: number }) => [n.node, n.fits])).toEqual([
      ["small", 0],
      ["large", 1],
    ]);
    expect(got.free_nodes[0].free.cpu).toBeCloseTo(1.92);
    expect(got.free_nodes[1].free).toEqual({ cpu: 4.91, memory: 15 * GiB });
    // (16 - 8) / 3 and (64 - 32) GiB / 15 GiB both floor to 2.
    expect(got.new_pods).toBe(2);
    expect(got.room).toBe(3);
  });

  test("is bounded by the tighter of the cpu and memory limits, never below 0", () => {
    expect(
      room(nodePool([cpuFloor], { cpu: "16", memory: "40Gi" }, { cpu: "8", memory: "32Gi" }))
        .new_pods
    ).toBe(0);
    expect(room(nodePool([cpuFloor], { cpu: "6" }, { cpu: "8" })).new_pods).toBe(0);
    expect(room(nodePool([cpuFloor], { cpu: "6" }, { cpu: "8" })).room).toBe(1);
  });

  test("bounds nothing when the pool sets no limit", () => {
    const got = room(nodePool([cpuFloor]));
    expect(got.new_pods).toBeNull();
    expect(got.room).toBeNull();
  });
});

describe("two_pods_per_instance", () => {
  const decide = (requirements: object[], pod = defaultPod) =>
    jq("two_pods_per_instance($pod)", nodePool(requirements), [
      "--argjson",
      "pod",
      JSON.stringify(pod),
    ]);

  test("says no node holds two when the instance-memory bound is under two pods' worth", () => {
    const got = decide([
      cpuFloor,
      requirement("karpenter.k8s.aws/instance-cpu", "Lt", "9"),
      requirement("karpenter.k8s.aws/instance-memory", "Lt", "17000"),
    ]);
    expect(got.fit).toBe(false);
    expect(got.reason).toBe(
      "the NodePool allows at most 16999 MiB an instance, under the 30720 two pods reserve, so no node holds two of the run's pods"
    );
  });

  test("says no node holds two when the instance-cpu bound is under two pods' worth, an In list included", () => {
    const got = decide([cpuFloor, requirement("karpenter.k8s.aws/instance-cpu", "In", "4", "5")]);
    expect(got.fit).toBe(false);
    expect(got.reason).toBe(
      "the NodePool allows at most 5 vCPUs an instance, under the 6 two pods reserve, so no node holds two of the run's pods"
    );
  });

  test("says the pods may share a node when both bounds hold two", () => {
    const got = decide([
      cpuFloor,
      requirement("karpenter.k8s.aws/instance-cpu", "Lt", "17"),
      requirement("karpenter.k8s.aws/instance-memory", "Lt", "65537"),
    ]);
    expect(got.fit).toBe(true);
    expect(got.reason).toBe(
      "the NodePool allows instances of up to 16 vCPUs and 65536 MiB, room for two pods (6 vCPUs, 30720 MiB) on paper, so the pods may share a node"
    );
  });

  test("decides nothing when a dimension has no maximum", () => {
    expect(decide([cpuFloor])).toEqual({
      fit: null,
      reason:
        "the NodePool bounds no maximum instance-cpu, so whether two of the run's pods (6 vCPUs, 30720 MiB) can share a node cannot be read from it",
    });
    expect(
      decide([cpuFloor, requirement("karpenter.k8s.aws/instance-cpu", "Lt", "17")]).reason
    ).toBe(
      "the NodePool bounds no maximum instance-memory, so whether two of the run's pods (6 vCPUs, 30720 MiB) can share a node cannot be read from it"
    );
  });
});
