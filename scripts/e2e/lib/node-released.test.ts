import { describe, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// node-release (stage4b-sandbox-tree.sh) waits for every node a pod of tree 1 ran on to be
// released: gone, or carrying no pod of tree 1 while another pod keeps it. node_pods_judged is the
// judgement over the node's pods, as `kubectl get pods -o json` lists them, and node_released reads
// the node and its pods through kubectl and judges them. The run at a578760e failed the checkpoint
// with the node holding the run's repository cache pod (`legion.dev/cache=true`, no tree label,
// alive as long as the daemon) and the carried child's new tree's pod (another tree of the same
// project, launched at tree 1's linger expiry), neither of them tree 1's: the judgement must let a
// node stay for those and for another project's pod, hold the wait for a pod of tree 1 or of the
// run with no tree label, and count a finished pod or a pod with no project label for nothing.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));

const run = "legsmoke";
const tree1 = "LEGSMOKE-852";

interface Pod {
  readonly name: string;
  readonly phase?: string;
  readonly labels?: Record<string, string>;
}

const pod = (name: string, labels?: Record<string, string>, phase = "Running"): Pod => ({
  name,
  phase,
  labels,
});
const runPod = (name: string, tree: string, phase?: string) =>
  pod(name, { "legion.dev/project": run, "legion.dev/tree": tree }, phase);

const listing = (pods: readonly Pod[]) =>
  JSON.stringify({
    items: pods.map((p) => ({
      metadata: { name: p.name, labels: p.labels ?? {} },
      status: { phase: p.phase ?? "Running" },
    })),
  });

const cache = pod("legion-legsmoke-cache-sjawhar-legion-smoke-6687cbae", {
  "legion.dev/project": run,
  "legion.dev/cache": "true",
});
const carriedChild = runPod("legion-legsmoke-legsmoke-860", "LEGSMOKE-860");
const otherProject = pod("legion-acme-acme-856", {
  "legion.dev/project": "acme",
  "legion.dev/tree": "ACME-856",
});
const daemonSet = pod("cilium-abcde");

function judged(pods: readonly Pod[]): { holding: string[]; others: string[] } {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -euo pipefail
run_label=${run} tree1=${tree1}
${fn("node_pods_judged")}
node_pods_judged <<<"$LISTING"`,
    ],
    { env: { PATH: process.env.PATH ?? "", LISTING: listing(pods) } }
  );
  expect(result.stderr.toString()).toBe("");
  return JSON.parse(result.stdout.toString()) as { holding: string[]; others: string[] };
}

describe("node_pods_judged", () => {
  test("the node the run at a578760e held: the cache, the carried child's new tree and another project keep it, none of them tree 1's", () => {
    expect(judged([cache, carriedChild, otherProject, daemonSet])).toEqual({
      holding: [],
      others: [
        "legsmoke/legion-legsmoke-cache-sjawhar-legion-smoke-6687cbae Running",
        "legsmoke/legion-legsmoke-legsmoke-860 Running",
        "acme/legion-acme-acme-856 Running",
      ],
    });
  });

  test("a pod of tree 1, Pending or Running, holds the wait, as does the run's pod with no tree label other than the cache", () => {
    const root = runPod("legion-legsmoke-legsmoke-852", tree1, "Pending");
    const child3 = runPod("legion-legsmoke-legsmoke-860", tree1);
    const untreed = pod("legion-legsmoke-controller", { "legion.dev/project": run });
    expect(judged([root, child3, untreed, cache, otherProject])).toEqual({
      holding: [
        "legion-legsmoke-legsmoke-852",
        "legion-legsmoke-legsmoke-860",
        "legion-legsmoke-controller",
      ],
      others: [
        "legsmoke/legion-legsmoke-cache-sjawhar-legion-smoke-6687cbae Running",
        "acme/legion-acme-acme-856 Running",
      ],
    });
  });

  test("a finished pod and a pod with no project label count for nothing, so a node with only those has nothing keeping it", () => {
    expect(
      judged([
        runPod("legion-legsmoke-legsmoke-852", tree1, "Succeeded"),
        pod("legion-acme-acme-856", otherProject.labels, "Failed"),
        daemonSet,
      ])
    ).toEqual({ holding: [], others: [] });
  });
});

describe("node_released, over a stub kubectl", () => {
  // The stub answers `get node` with what NODE_EXISTS says and `get pods` with LISTING; `timeout`
  // runs it from PATH, so it is a script, not a function.
  const bin = mkdtempSync(join(tmpdir(), "node-released-"));
  writeFileSync(
    join(bin, "kubectl"),
    `#!/usr/bin/env bash
case " $* " in
  *" get node "*) if [ "$NODE_EXISTS" = yes ]; then echo "node/$NODE"; fi ;;
  *" get pods "*) printf '%s' "$LISTING" ;;
  *) echo "unexpected kubectl $*" >&2; exit 2 ;;
esac
`
  );
  chmodSync(join(bin, "kubectl"), 0o755);

  function released(node: string, exists: boolean, pods: readonly Pod[]) {
    const result = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -uo pipefail
run_label=${run} tree1=${tree1} operator=op namespace=legion NODE=${node}
${fn("node_pods_judged")}
node_release=
${fn("node_released")}
if node_released "$NODE"; then echo "RELEASED $node_release"; else echo "WAITING"; fi`,
      ],
      {
        env: {
          PATH: `${bin}:${process.env.PATH ?? ""}`,
          LISTING: listing(pods),
          NODE_EXISTS: exists ? "yes" : "no",
        },
      }
    );
    expect(result.stderr.toString()).toBe("");
    return result.stdout.toString().trim();
  }

  test("a node that is gone is released; one that stays is released only once no pod of tree 1 is on it and another pod keeps it", () => {
    expect(released("ip-10-1-10-42", false, [])).toBe("RELEASED node ip-10-1-10-42 is gone");
    expect(released("ip-10-1-11-161", true, [cache, carriedChild, otherProject, daemonSet])).toBe(
      "RELEASED node ip-10-1-11-161 stays, carrying no pod of tree 1 while other pods keep it (legsmoke/legion-legsmoke-cache-sjawhar-legion-smoke-6687cbae Running, legsmoke/legion-legsmoke-legsmoke-860 Running, acme/legion-acme-acme-856 Running)"
    );
    // Tree 1's pod still on it: the wait goes on, whatever else keeps the node.
    expect(
      released("ip-10-1-11-161", true, [runPod("legion-legsmoke-legsmoke-852", tree1), cache])
    ).toBe("WAITING");
    // Nothing keeps it: the pool has not taken it yet, so the wait goes on.
    expect(released("ip-10-1-10-52", true, [daemonSet])).toBe("WAITING");
  });
});
