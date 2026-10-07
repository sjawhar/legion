import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { runJq } from "./run-jq";

// The pod shape's --connect rule: Stage 4b's own shape_problems, taken from the script by name as
// stage4b-verdict.test.ts takes its functions, run on the runtime's golden root pod, whose worker
// shim dials tcp://192.0.2.250:13371 (packages/daemon/internal/runtime/sandbox/testdata/golden).
const root = join(import.meta.dir, "..", "..", "..");
const lib = join(root, "scripts", "e2e", "lib");
const script = readFileSync(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"), "utf8");
const shapeProblems = /^shape_problems\(\) \{[\s\S]*?\n\}$/m.exec(script)?.[0];
if (shapeProblems === undefined)
  throw new Error("stage4b-sandbox-tree.sh defines no shape_problems()");
const golden = JSON.parse(
  readFileSync(
    join(
      root,
      "packages",
      "daemon",
      "internal",
      "runtime",
      "sandbox",
      "testdata",
      "golden",
      "root.json"
    ),
    "utf8"
  )
);
const goldenStream = "tcp://192.0.2.250:13371";

interface Container {
  name: string;
  command?: string[];
}
// pod is the golden root pod, its worker container's command edited by argv.
function pod(argv: (command: string[]) => string[] = (command) => command) {
  const spec = structuredClone(golden.spec.podTemplate.spec);
  spec.containers = spec.containers.map((container: Container) =>
    container.name === "worker"
      ? { ...container, command: argv(container.command ?? []) }
      : container
  );
  return { metadata: { name: "pod", uid: "uid" }, spec };
}

// connectProblems runs shape_problems on the pod with advertise_host HOST at the worker stream's
// port 13371 and prints only its --connect lines: the golden pod departs from the run's other rules
// (its route ConfigMap and audience are the golden's own), which are not under test here.
function connectProblems(object: object, host = "192.0.2.250"): string[] {
  const run = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)} route_configmap=route gateway_audience=audience host=${host} port_worker_stream=13371
${shapeProblems}
shape_problems`,
    ],
    { stdin: new TextEncoder().encode(JSON.stringify(object)) }
  );
  if (run.exitCode !== 0) throw new Error(`shape_problems exited ${run.exitCode}: ${run.stderr}`);
  return run.stdout
    .toString()
    .split("\n")
    .filter((line) => line.includes("worker shim"));
}

describe("the pod shape's --connect rule", () => {
  test("a pod whose shim dials advertise_host at the worker stream port passes it", () => {
    expect(connectProblems(pod())).toEqual([]);
  });

  test("a pod told the address the daemon binds departs, naming that address", () => {
    const wildcard = pod((command) =>
      command.map((word, i) => (command[i - 1] === "--connect" ? "tcp://0.0.0.0:13371" : word))
    );
    expect(connectProblems(wildcard)).toEqual([
      `the worker shim dials tcp://0.0.0.0:13371, not advertise_host at ${goldenStream}`,
    ]);
  });

  test("a pod told another advertise_host departs", () => {
    expect(connectProblems(pod(), "192.0.2.9")).toEqual([
      `the worker shim dials ${goldenStream}, not advertise_host at tcp://192.0.2.9:13371`,
    ]);
  });

  test("a pod whose shim has no --connect departs", () => {
    const unconnected = pod((command) =>
      command.filter((word, i) => word !== "--connect" && command[i - 1] !== "--connect")
    );
    expect(connectProblems(unconnected)).toEqual([
      `the worker shim dials nothing (no --connect), not advertise_host at ${goldenStream}`,
    ]);
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; shim_connect'], JSON.stringify(unconnected))
    ).toBe("null\n");
  });
});

describe("stage4b-pods.jq's ready_pods", () => {
  // event is one pod watch line: a pod with these labels whose worker is ready or not.
  function event(name: string, labels: object, ready: boolean) {
    return JSON.stringify({
      kind: "Pod",
      object: {
        kind: "Pod",
        metadata: { name, labels },
        status: { containerStatuses: [{ name: "worker", ready }] },
      },
    });
  }

  test("keeps a ready Sandbox pod and leaves out the probe, a control, and an unready pod", () => {
    const watch = [
      event("kept", {}, true),
      event("probe", { "legion.dev/probe": "x" }, true),
      event("control", { "legion.dev/e2e-control": "x" }, true),
      event("unready", {}, false),
    ].join("\n");
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; ready_pods | .metadata.name'], watch)
    ).toBe("kept\n");
  });
});
