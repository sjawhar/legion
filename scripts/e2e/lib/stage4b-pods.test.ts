import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runJq } from "./run-jq";
import { scriptFunctions } from "./script-functions";

// Stage 4b's reads of a pod's addresses: the pod shape's --connect rule (shape_problems and
// record_stream) and the address-moved checkpoints' readers (claims_on_handed_addresses and the
// helpers restart-mid-tree's negative control and the two moves use), taken from the script by name
// (script-functions.ts) and run on the runtime's golden root issue pod, whose six role launchers dial
// tcp://192.0.2.250:13371 (packages/daemon/internal/runtime/sandbox/testdata/golden).
const root = join(import.meta.dir, "..", "..", "..");
const lib = join(root, "scripts", "e2e", "lib");
const fn = scriptFunctions(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"));
const shapeProblems = fn("shape_problems");
const dir = mkdtempSync(join(tmpdir(), "stage4b-pods-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
let runs = 0;
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
// The issue pod's role containers, in the order the runtime builds them (claim.Roles).
const roles = ["architect", "planner", "implementer", "tester", "reviewer", "merger"];
const goldenStream = "tcp://192.0.2.250:13371";
// A moved stream: a restart on the run's other port, so the worker stream takes the API's.
const movedStream = "tcp://192.0.2.250:13370";
const created = "2026-10-06T12:00:00Z";
const moved = "2026-10-06T12:30:00Z";

interface Container {
  name: string;
  command?: string[];
}
// withConnect is an argv edit that points a role launcher's --connect at STREAM.
const withConnect = (stream: string) => (command: string[]) =>
  command.map((word, i) => (command[i - 1] === "--connect" ? stream : word));
// pod is the golden root issue pod created at AT, the command of each role container ONLY names
// (every role's by default) edited by argv.
function pod(
  argv: (command: string[]) => string[] = (command) => command,
  at = created,
  only: string[] = roles
) {
  const spec = structuredClone(golden.spec.podTemplate.spec);
  spec.containers = spec.containers.map((container: Container) =>
    only.includes(container.name)
      ? { ...container, command: argv(container.command ?? []) }
      : container
  );
  return { metadata: { name: "pod", uid: "uid", creationTimestamp: at }, spec };
}
// eachLauncher is the line shape_problems prints for every role launcher, in container order.
const eachLauncher = (line: (role: string) => string) => roles.map(line);

interface Stream {
  since: string;
  stream: string;
}
interface Run {
  // host is advertise_host; port is the worker stream port the calling shell holds now (the shape
  // watcher forked before a restart that moved the stream holds the old one, the main shell after
  // it the new one).
  host?: string;
  port?: number;
  // streams is what record_stream wrote as each daemon started; by default one daemon serving
  // advertise_host at the port since before the pod was created.
  streams?: Stream[];
}
// problems runs shape_problems on the pod under RUN's worker streams and returns every line it
// prints; the filters below each keep the lines of one rule, since the golden pod departs from the
// run's other rules (its route ConfigMap and audience are the golden's own).
function problems(object: object, run: Run = {}): string[] {
  const host = run.host ?? "192.0.2.250";
  const port = run.port ?? 13371;
  const evidence = join(dir, `run-${++runs}`);
  const streams = run.streams ?? [
    { since: "2026-10-06T11:00:00Z", stream: `tcp://${host}:${port}` },
  ];
  mkdirSync(evidence);
  writeFileSync(
    join(evidence, "worker-streams.jsonl"),
    streams.map((s) => `${JSON.stringify(s)}\n`).join("")
  );
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)} evidence=${JSON.stringify(evidence)} route_configmap=route gateway_audience=audience host=${host} port_worker_stream=${port}
${shapeProblems}
shape_problems`,
    ],
    { stdin: new TextEncoder().encode(JSON.stringify(object)) }
  );
  if (result.exitCode !== 0)
    throw new Error(`shape_problems exited ${result.exitCode}: ${result.stderr}`);
  return result.stdout.toString().split("\n");
}
// connectProblems is the --connect rule's lines alone.
function connectProblems(object: object, run: Run = {}): string[] {
  return problems(object, run).filter(
    (line) => line.includes("launcher dials") || line.includes("launchers dial")
  );
}

describe("the pod shape's --connect rule", () => {
  test("a pod whose role launchers dial advertise_host at the worker stream port passes it", () => {
    expect(connectProblems(pod())).toEqual([]);
  });

  test("a pod told the address the daemon binds departs, naming each launcher and that address", () => {
    const wildcard = pod(withConnect("tcp://0.0.0.0:13371"));
    expect(connectProblems(wildcard)).toEqual(
      eachLauncher(
        (role) =>
          `the ${role} launcher dials tcp://0.0.0.0:13371, not advertise_host at ${goldenStream}`
      )
    );
  });

  test("one launcher told another address departs alone, naming its role", () => {
    const stray = pod(withConnect("tcp://192.0.2.9:13371"), created, ["reviewer"]);
    expect(connectProblems(stray)).toEqual([
      `the reviewer launcher dials tcp://192.0.2.9:13371, not advertise_host at ${goldenStream}`,
    ]);
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'], JSON.stringify(stray))
    ).toBe("null\n");
  });

  test("a pod told another advertise_host departs", () => {
    expect(connectProblems(pod(), { host: "192.0.2.9" })).toEqual(
      eachLauncher(
        (role) =>
          `the ${role} launcher dials ${goldenStream}, not advertise_host at tcp://192.0.2.9:13371`
      )
    );
  });

  test("a pod whose launchers have no --connect departs", () => {
    const unconnected = pod((command) =>
      command.filter((word, i) => word !== "--connect" && command[i - 1] !== "--connect")
    );
    expect(connectProblems(unconnected)).toEqual(
      eachLauncher(
        (role) =>
          `the ${role} launcher dials nothing (no --connect), not advertise_host at ${goldenStream}`
      )
    );
    expect(
      runJq(
        ["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'],
        JSON.stringify(unconnected)
      )
    ).toBe("null\n");
  });

  test("launcher_connect is the one address every role launcher dials", () => {
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'], JSON.stringify(pod()))
    ).toBe(`${goldenStream}\n`);
  });
});

// ghProblems runs shape_problems on the pod and prints only its gh credential lines (LEGION-631):
// each role's gh-<role> volume, mount and the clone's helper.
function ghProblems(object: object): string[] {
  return problems(object).filter((line) =>
    /gh-|credential-helper|shareProcessNamespace/.test(line)
  );
}

describe("the pod shape's gh credential rule", () => {
  test("the golden issue pod projects each role's gh files into its own container alone", () => {
    expect(ghProblems(pod())).toEqual([]);
  });

  test("a container mounting another role's gh volume departs, naming both", () => {
    const crossed = pod();
    const implementer = crossed.spec.containers.find((c: Container) => c.name === "implementer");
    implementer.volumeMounts.push({
      name: "gh-reviewer",
      mountPath: "/var/run/legion/gh-reviewer",
      readOnly: true,
    });
    expect(ghProblems(crossed)).toEqual([
      "container implementer mounts gh volumes gh-implementer at /var/run/legion/gh read-only, gh-reviewer at /var/run/legion/gh-reviewer read-only, want gh-implementer alone, read-only at /var/run/legion/gh",
      "container implementer mounts gh-reviewer under /var/run/legion/gh",
    ]);
  });

  test("a gh volume of another Secret, a missing file, a shared pid namespace and the old helper each depart", () => {
    const departed = pod();
    departed.spec.volumes.find((v: { name: string }) => v.name === "gh-merger").secret.secretName =
      "other";
    departed.spec.volumes.find((v: { name: string }) => v.name === "gh-tester").secret.items.pop();
    departed.spec.shareProcessNamespace = true;
    const init = departed.spec.initContainers.find((c: Container) => c.name === "workspace-init");
    init.command[init.command.indexOf("--credential-helper") + 1] =
      "!/opt/legion/bin/legion credential";
    expect(ghProblems(departed)).toEqual([
      'gh-tester projects [{"key":"github-hosts","path":"hosts.yml"}], not hosts.yml and config.yml',
      "gh-merger projects Secret other, not the role Secret legion-legion-legion-208-merger-boot its launcher token comes from",
      "shareProcessNamespace is set",
      "the --credential-helper of workspace-init is !/opt/legion/bin/legion credential, not !gh auth git-credential",
    ]);
  });
});

describe("the pod shape across a move of the worker stream", () => {
  // The run's daemons as record_stream noted them: the first on the golden stream, a later one on
  // the moved stream.
  const streams = [
    { since: "2026-10-06T11:00:00Z", stream: goldenStream },
    { since: moved, stream: movedStream },
  ];
  const later = "2026-10-06T12:31:00Z";

  test("the main shell, on the moved port, holds a pod created before the move to the old stream", () => {
    expect(connectProblems(pod(), { port: 13370, streams })).toEqual([]);
  });

  test("the shape watcher, forked on the old port, holds a pod created after it to the new stream", () => {
    expect(connectProblems(pod(withConnect(movedStream), later), { port: 13371, streams })).toEqual(
      []
    );
  });

  test("a pod created in the move's second is held to the new stream", () => {
    expect(connectProblems(pod(withConnect(movedStream), moved), { streams })).toEqual([]);
  });

  test("a pod created after the move but told the old stream departs, naming the new one", () => {
    expect(connectProblems(pod(undefined, later), { port: 13371, streams })).toEqual(
      eachLauncher(
        (role) => `the ${role} launcher dials ${goldenStream}, not advertise_host at ${movedStream}`
      )
    );
  });

  test("a pod created before the move but told the new stream departs, naming the old one", () => {
    expect(connectProblems(pod(withConnect(movedStream)), { port: 13370, streams })).toEqual(
      eachLauncher(
        (role) => `the ${role} launcher dials ${movedStream}, not advertise_host at ${goldenStream}`
      )
    );
  });

  test("a pod created before any daemon served a stream departs, naming when it was created", () => {
    expect(connectProblems(pod(undefined, "2026-10-06T10:59:59Z"), { streams })).toEqual([
      `the role launchers dial ${goldenStream}, but no worker stream was served when the pod was created (2026-10-06T10:59:59Z)`,
    ]);
  });
});

describe("record_stream", () => {
  // recordStreams runs record_stream once per port, each as a daemon on that worker stream port is
  // about to start, and returns the record and the second each record_stream call began in.
  function recordStreams(ports: number[]) {
    const evidence = join(dir, `run-${++runs}`);
    mkdirSync(evidence);
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
evidence=${JSON.stringify(evidence)} host=192.0.2.250
${fn("record_stream")}
for port_worker_stream in ${ports.join(" ")}; do date -u +%FT%TZ; record_stream; done`,
    ]);
    if (result.exitCode !== 0)
      throw new Error(`record_stream exited ${result.exitCode}: ${result.stderr}`);
    return {
      began: result.stdout.toString().trim().split("\n"),
      record: readFileSync(join(evidence, "worker-streams.jsonl"), "utf8")
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line) as Stream),
    };
  }

  test("notes each daemon's stream, a restart at the same address included", () => {
    const { record } = recordStreams([13371, 13371]);
    expect(record.map((r) => r.stream)).toEqual([goldenStream, goldenStream]);
  });

  test("starts a moved stream in a later second than any pod the stopped daemon created", () => {
    // The second the move began in is the latest a pod the stopped daemon created can carry.
    const { began, record } = recordStreams([13371, 13370]);
    expect(record.map((r) => r.stream)).toEqual([goldenStream, movedStream]);
    expect(record[1].since > began[1]).toBe(true);
  });
});

// runScript runs SCRIPT under bash with errexit and pipefail, as stage4b-sandbox-tree.sh runs.
function runScript(script: string) {
  const result = Bun.spawnSync(["bash", "-c", `set -Eeuo pipefail\n${script}`]);
  return { exitCode: result.exitCode, out: result.stdout.toString() };
}

describe("the address-moved checkpoints' readers", () => {
  // The run after address-moved-stream-new-pod: its two ports swapped, so the worker stream is the
  // API's old port and the API the stream's.
  const movedDaemonURL = "http://192.0.2.250:13371";
  const dispatch = "https://dispatch.internal.example";
  const envoy = "https://envoy.internal.example:8443";
  const nats = "nats://nats.internal.example:4222";
  // handedEnv is what a role generation launched now is told, edited by VALUES.
  const handedEnv = (values: Record<string, string> = {}) => ({
    DISPATCH_URL: dispatch,
    ENVOY_URL: envoy,
    ENVOY_NATS_URL: nats,
    LEGION_DAEMON_URL: movedDaemonURL,
    ...values,
  });
  const movedPod = () => pod(withConnect(movedStream));
  interface Judged {
    pods: Record<string, object>;
    // envs is each role's Oh My Pi environment, keyed by "<pod>/<role>"; a role with none has no
    // Oh My Pi to read.
    envs: Record<string, Record<string, string>>;
    claims: { token: string; pod: string; role: string }[] | "fails";
    envoySource?: string;
    handedEnvoy?: string;
  }
  // judge runs the script's claims_on_handed_addresses over PODS (the operator's reads by pod
  // name), ENVS (pod_env's reads) and CLAIMS (what `legion claims` shows, or a read that fails),
  // and returns its exit code, what it printed, and how many times it read `legion claims`.
  function judge(j: Judged) {
    const run = join(dir, `run-${++runs}`);
    mkdirSync(run);
    writeFileSync(join(run, "pods.json"), JSON.stringify(j.pods));
    writeFileSync(join(run, "envs.json"), JSON.stringify(j.envs));
    writeFileSync(join(run, "claims.json"), JSON.stringify(j.claims === "fails" ? [] : j.claims));
    const result =
      runScript(`root=${JSON.stringify(root)} run=${JSON.stringify(run)} host=192.0.2.250 port_daemon=13371 port_worker_stream=13370
dispatch_base=${dispatch} nats_url=${nats} handed_envoy_url=${j.handedEnvoy ?? envoy} handed_envoy_source=${JSON.stringify(j.envoySource ?? "LEGION_E2E_ENVOY_URL")}
note() { echo "   $*"; }
fail() { echo "FAIL: $*"; exit 1; }
scrub() { cat; }
op() { [ "$1 $2 $4 $5" = "get pod -o json" ] && jq -e --arg p "$3" '.[$p]' "$run/pods.json"; }
pod_env() { jq -er --arg k "$1/$2" '.[$k] | to_entries[] | "\\(.key)=\\(.value)"' "$run/envs.json"; }
live_claims() { echo read >>"$run/reads"; ${j.claims === "fails" ? "return 1" : 'jq -c . "$run/claims.json"'}; }
${fn("pod_connect")}
${fn("pod_endpoint_mismatch")}
${fn("on_handed_addresses")}
${fn("claims_on_handed_addresses")}
claims_on_handed_addresses`);
    let reads = 0;
    try {
      reads = readFileSync(join(run, "reads"), "utf8").trim().split("\n").length;
    } catch {}
    return { exitCode: result.exitCode, out: result.out, reads };
  }
  const claims = [
    { token: "claim-a", pod: "pod-a", role: "architect" },
    { token: "claim-b", pod: "pod-a", role: "tester" },
  ];
  const handed = { "pod-a/architect": handedEnv(), "pod-a/tester": handedEnv() };

  test("pod_connect is one line, printing the one address every role launcher dials", () => {
    expect(fn("pod_connect").split("\n")).toHaveLength(1);
    const pods = {
      connected: pod(),
      stray: pod(withConnect("tcp://192.0.2.9:13371"), created, ["reviewer"]),
    };
    const { exitCode, out } = runScript(`root=${JSON.stringify(root)}
op() { jq -e --arg p "$3" '.[$p]' <<<${JSON.stringify(JSON.stringify(pods))}; }
${fn("pod_connect")}
printf '%s|%s\\n' "$(pod_connect connected)" "$(pod_connect stray)"
if absent=$(pod_connect absent); then echo "absent read: $absent"; fi`);
    expect(exitCode).toBe(0);
    expect(out).toBe(`${goldenStream}|\n`);
  });

  test("every claim on the handed addresses passes, judged from one read of legion claims", () => {
    const { exitCode, out, reads } = judge({ pods: { "pod-a": movedPod() }, envs: handed, claims });
    expect(exitCode).toBe(0);
    expect(reads).toBe(1);
    expect(out).toContain(
      `claim-b: pod pod-a's launchers dial ${movedStream}; its tester is told LEGION_DAEMON_URL ${movedDaemonURL} and the run's services`
    );
    expect(out).toContain(
      "all 2 claims that run a process are on the addresses the daemon hands now"
    );
  });

  test("a pod whose launchers keep the old --connect fails, naming the claim, role and pod", () => {
    const { exitCode, out } = judge({ pods: { "pod-a": pod() }, envs: handed, claims });
    expect(exitCode).toBe(1);
    expect(out).toEndWith(
      `FAIL: claim-a's architect in pod pod-a's role launchers dial ${goldenStream}, not ${movedStream}\n`
    );
  });

  test("a pod one of whose launchers dials elsewhere fails, naming no one address", () => {
    const split = pod(withConnect(movedStream), created, roles.slice(1));
    const { exitCode, out } = judge({ pods: { "pod-a": split }, envs: handed, claims });
    expect(exitCode).toBe(1);
    expect(out).toEndWith(
      `FAIL: claim-a's architect in pod pod-a's role launchers dial no one address, not ${movedStream}\n`
    );
  });

  test("a role told the old API fails, naming it", () => {
    const envs = {
      ...handed,
      "pod-a/tester": handedEnv({ LEGION_DAEMON_URL: "http://192.0.2.250:13370" }),
    };
    const { exitCode, out } = judge({ pods: { "pod-a": movedPod() }, envs, claims });
    expect(exitCode).toBe(1);
    expect(out).toEndWith(
      `FAIL: claim-b's tester in pod pod-a has LEGION_DAEMON_URL=http://192.0.2.250:13370, want ${movedDaemonURL}\n`
    );
  });

  test("a role told another Envoy fails, naming the run's input, not the address", () => {
    const envs = { ...handed, "pod-a/architect": handedEnv({ ENVOY_URL: "https://192.0.2.9" }) };
    const { exitCode, out } = judge({ pods: { "pod-a": movedPod() }, envs, claims });
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: claim-a's architect in pod pod-a has ENVOY_URL differs from LEGION_E2E_ENVOY_URL\n"
    );
  });

  test("after the respelling, a role still told the old spelling fails, naming the respelled input", () => {
    const respelled = "https://ENVOY.INTERNAL.EXAMPLE:8443";
    const envs = { ...handed, "pod-a/architect": handedEnv({ ENVOY_URL: respelled }) };
    const { exitCode, out } = judge({
      pods: { "pod-a": movedPod() },
      envs,
      claims,
      handedEnvoy: respelled,
      envoySource: "LEGION_E2E_ENVOY_URL with its host upper-cased",
    });
    expect(exitCode).toBe(1);
    expect(out).not.toContain("internal.example");
    expect(out).toEndWith(
      "FAIL: claim-b's tester in pod pod-a has ENVOY_URL differs from LEGION_E2E_ENVOY_URL with its host upper-cased\n"
    );
  });

  test("a pod the operator cannot read fails, naming the pod", () => {
    const { exitCode, out } = judge({
      pods: { "pod-a": movedPod() },
      envs: handed,
      claims: [...claims, { token: "claim-c", pod: "pod-c", role: "planner" }],
    });
    expect(exitCode).toBe(1);
    expect(out).toEndWith("FAIL: the operator could not read pod pod-c\n");
  });

  test("a role with no readable Oh My Pi fails, naming its container", () => {
    const { exitCode, out } = judge({
      pods: { "pod-a": movedPod() },
      envs: { "pod-a/architect": handedEnv() },
      claims,
    });
    expect(exitCode).toBe(1);
    expect(out).toContain(
      "FAIL: claim-b's tester in pod pod-a has no readable Oh My Pi environment in its tester container"
    );
  });

  test("a legion claims read that fails fails the check, judging nothing", () => {
    const { exitCode, out, reads } = judge({
      pods: { "pod-a": movedPod() },
      envs: handed,
      claims: "fails",
    });
    expect(exitCode).toBe(1);
    expect(reads).toBe(1);
    expect(out).toBe("FAIL: legion claims could not be read\n");
  });

  test("a read that shows no claim running a process fails rather than passing empty", () => {
    const { exitCode, out } = judge({ pods: {}, envs: {}, claims: [] });
    expect(exitCode).toBe(1);
    expect(out).toBe("FAIL: legion claims shows no claim that runs a process\n");
  });

  test("upper_host upper-cases the authority alone", () => {
    const { out } = runScript(`${fn("upper_host")}
upper_host ${envoy}
upper_host http://envoy.internal.example`);
    expect(out).toBe("https://ENVOY.INTERNAL.EXAMPLE:8443\nhttp://ENVOY.INTERNAL.EXAMPLE\n");
  });

  test("scrub hides a production host however it is spelled", () => {
    const { out } =
      runScript(`service_hosts=(dispatch.internal.example envoy.internal.example:8443 nats.internal.example gateway.internal.example)
${fn("scrub")}
printf 'dial https://ENVOY.INTERNAL.EXAMPLE:8443: refused; envoy.internal.example too\\n' | scrub`);
    expect(out).toBe(
      "dial https://<LEGION_E2E_ENVOY_URL>:8443: refused; <LEGION_E2E_ENVOY_URL> too\n"
    );
  });

  describe("claims_relaunched", () => {
    const claim = (token: string, generation: number, uid = "uid-1") => ({
      token,
      generation,
      incarnation: `${uid}/${generation}`,
    });
    const relaunched = (before: object[], after: object[]) =>
      JSON.parse(
        runScript(`${fn("claims_relaunched")}
claims_relaunched ${JSON.stringify(JSON.stringify(before))} ${JSON.stringify(JSON.stringify(after))}`)
          .out
      );
    const before = [claim("a", 1), claim("b", 3)];

    test("lists nothing when every claim runs as it did", () => {
      expect(relaunched(before, before)).toEqual([]);
    });

    test("lists a claim at its next generation and one in another pod, with what each was", () => {
      expect(relaunched(before, [claim("a", 2), claim("b", 3, "uid-2")])).toEqual([
        {
          token: "a",
          generation: 2,
          incarnation: "uid-1/2",
          was: { generation: 1, incarnation: "uid-1/1" },
        },
        {
          token: "b",
          generation: 3,
          incarnation: "uid-2/3",
          was: { generation: 3, incarnation: "uid-1/3" },
        },
      ]);
    });

    test("does not list a claim that runs no process now", () => {
      expect(relaunched(before, [claim("a", 1)])).toEqual([]);
    });
  });

  test("pod_resume prints the one --resume word the role's processes carry, or nothing", () => {
    const { out } = runScript(`pod_commands() {
  case "$2" in
    tester) printf '%s\\n' "legion launcher --role tester" "legion worker-shim -- omp --resume=/s/a.jsonl --mode rpc" "omp --resume=/s/a.jsonl --mode rpc" ;;
    planner) printf '%s\\n' "legion launcher --role planner" ;;
  esac
}
${fn("pod_resume")}
printf '%s|%s\\n' "$(pod_resume pod tester)" "$(pod_resume pod planner)"`);
    expect(out).toBe("--resume=/s/a.jsonl|\n");
  });
});

describe("stage4b-pods.jq's ready_pods", () => {
  // event is one pod watch line: a pod with these labels whose role launchers are ready, all but
  // the one NOT_READY names.
  function event(name: string, labels: object, notReady?: string) {
    return JSON.stringify({
      kind: "Pod",
      object: {
        kind: "Pod",
        metadata: { name, labels },
        status: {
          containerStatuses: roles.map((role) => ({ name: role, ready: role !== notReady })),
        },
      },
    });
  }

  test("keeps a ready issue pod and leaves out the probe, a control, and a pod with a role unready", () => {
    const watch = [
      event("kept", {}),
      event("probe", { "legion.dev/probe": "x" }),
      event("control", { "legion.dev/e2e-control": "x" }),
      event("unready", {}, "merger"),
    ].join("\n");
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; ready_pods | .metadata.name'], watch)
    ).toBe("kept\n");
  });
});
