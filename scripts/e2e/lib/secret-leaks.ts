// Whether any pod of a run carried a value of its Sandbox's Secrets in a container's command, args
// or environment, judged against every value those Secrets held for the whole run. A pod's -boot
// Secret gets the next generation's token at each relaunch, and the pod watch records no Secret
// value, so a check made after the fact reads the wrong values or none. This watches the run's
// Secrets from the start and keeps each value it sees in memory only: no value is printed or
// written anywhere. On SIGTERM it reads the pod watch the driver recorded and writes the verdict.
//
//   bun scripts/e2e/lib/secret-leaks.ts <context> <namespace> <label-selector> <pod-watch> <verdict>
//     watches the namespace's Secrets that carry the label selector until SIGTERM, then writes to
//     <verdict> one JSON line, {"pods","secrets","values","leaks","unseen"}: leaks is each pod in
//     <pod-watch> (one watch event a line) whose worker ever ran ready and one of whose containers
//     carries a value <pod>-boot held, as {"uid","pod","secret"}; unseen is each such pod whose
//     <pod>-boot the watch never saw, since its check would have judged nothing. It exits 0, or 2
//     when it cannot start
//
// The watch resumes from the last resourceVersion it saw when the API server ends it, and lists the
// Secrets again when the server no longer holds that version, so no value a Secret held is missed.
import { type ChildProcessByStdio, spawn } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { createInterface } from "node:readline";
import type { Readable } from "node:stream";

function refuse(message: string): never {
  process.stderr.write(`secret-leaks: ${message}\n`);
  process.exit(2);
}

const [context, namespace, selector, podWatch, verdict] = process.argv.slice(2);
if (!context || !namespace || !selector || !podWatch || !verdict) {
  refuse("usage: <context> <namespace> <label-selector> <pod-watch> <verdict>");
}

interface SecretObject {
  metadata?: { name?: string; resourceVersion?: string };
  data?: Record<string, string>;
}

// Every value each Secret has held, by the Secret's name.
const seen = new Map<string, Set<string>>();
function remember(secret: SecretObject): void {
  const name = secret.metadata?.name;
  if (!name) return;
  const values = seen.get(name) ?? new Set<string>();
  for (const encoded of Object.values(secret.data ?? {})) {
    const value = Buffer.from(encoded, "base64").toString("utf8");
    if (value !== "") values.add(value);
  }
  seen.set(name, values);
}

const collection = `/api/v1/namespaces/${namespace}/secrets?labelSelector=${encodeURIComponent(selector)}`;
type Kubectl = ChildProcessByStdio<null, Readable, null>;
function kubectl(path: string): Kubectl {
  return spawn("kubectl", ["--context", context, "get", "--raw", path], {
    stdio: ["ignore", "pipe", "ignore"],
  });
}

async function list(): Promise<string> {
  const child = kubectl(collection);
  let body = "";
  for await (const chunk of child.stdout) body += chunk;
  const closed = Promise.withResolvers<number>();
  child.on("close", closed.resolve);
  const code = await closed.promise;
  if (code !== 0) return "";
  const parsed = JSON.parse(body) as {
    metadata: { resourceVersion: string };
    items: SecretObject[];
  };
  for (const item of parsed.items) remember(item);
  return parsed.metadata.resourceVersion;
}

let stopping = false;
let current: Kubectl | undefined;

async function watch(): Promise<void> {
  let version = "";
  while (!stopping) {
    if (version === "") {
      version = await list();
      if (version === "") {
        await Bun.sleep(2000);
        continue;
      }
    }
    current = kubectl(`${collection}&watch=1&allowWatchBookmarks=true&resourceVersion=${version}`);
    for await (const line of createInterface({ input: current.stdout })) {
      if (line.trim() === "") continue;
      const event = JSON.parse(line) as { type: string; object: SecretObject & { code?: number } };
      if (event.type === "ERROR") {
        // 410 Gone: the server no longer holds that version, so the Secrets are listed again.
        if (event.object.code === 410) version = "";
        break;
      }
      if (event.object.metadata?.resourceVersion) version = event.object.metadata.resourceVersion;
      if (event.type !== "BOOKMARK") remember(event.object);
    }
    current = undefined;
  }
}

interface Container {
  command?: string[];
  args?: string[];
  env?: { value?: string }[];
}
interface PodEvent {
  object?: {
    kind?: string;
    metadata?: { uid?: string; name?: string; labels?: Record<string, string> };
    spec?: { containers?: Container[]; initContainers?: Container[] };
    status?: { containerStatuses?: { name: string; ready: boolean }[] };
  };
}

function judge(): void {
  const pods = new Map<string, { name: string; words: Set<string> }>();
  for (const line of readFileSync(podWatch, "utf8").split("\n")) {
    if (line.trim() === "") continue;
    const pod = (JSON.parse(line) as PodEvent).object;
    const uid = pod?.metadata?.uid;
    const name = pod?.metadata?.name;
    if (pod?.kind !== "Pod" || !uid || !name) continue;
    const labels = pod.metadata?.labels ?? {};
    if (labels["legion.dev/probe"] || labels["legion.dev/e2e-control"]) continue;
    if (!pod.status?.containerStatuses?.some((c) => c.name === "worker" && c.ready)) continue;
    const entry = pods.get(uid) ?? { name, words: new Set<string>() };
    for (const c of [...(pod.spec?.initContainers ?? []), ...(pod.spec?.containers ?? [])]) {
      for (const word of [
        ...(c.command ?? []),
        ...(c.args ?? []),
        ...(c.env ?? []).map((e) => e.value ?? ""),
      ]) {
        if (word !== "") entry.words.add(word);
      }
    }
    pods.set(uid, entry);
  }
  const leaks: { uid: string; pod: string; secret: string }[] = [];
  const unseen: { uid: string; pod: string; secret: string }[] = [];
  let values = 0;
  for (const set of seen.values()) values += set.size;
  for (const [uid, { name, words }] of pods) {
    const secret = `${name}-boot`;
    const held = seen.get(secret) ?? new Set<string>();
    if (held.size === 0) unseen.push({ uid, pod: name, secret });
    if ([...held].some((value) => [...words].some((word) => word.includes(value)))) {
      leaks.push({ uid, pod: name, secret });
    }
  }
  writeFileSync(
    verdict,
    `${JSON.stringify({ pods: pods.size, secrets: seen.size, values, leaks, unseen })}\n`
  );
}

process.on("SIGTERM", () => {
  stopping = true;
  current?.kill("SIGKILL");
  judge();
  process.exit(0);
});

await watch();
