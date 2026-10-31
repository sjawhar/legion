import { afterAll, describe, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  watch,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// lib/secret-leaks.ts, Stage 4b's leak check (pod-shape): it watches the run's Secrets and, on
// SIGTERM, flags every ready Sandbox pod one of whose containers' command, args or env values
// carries a value of its Sandbox's Secrets. A role Secret holds the role's launcher token
// (LAUNCHER_TOKEN, names.go) and its gh bundle (`github-hosts-<owner>`, `github-config-<owner>`),
// and since #2008 also `github-default`, the bundle's default owner — a plain name every issue
// pod's workspace-fetch and workspace-init args carry as `<owner>/<repo>`. The run at 90a60026
// failed pod-shape on that: 19 ready pods × 6 role Secrets = 114 leaks, every one the owner name,
// none a credential. The check must judge only the values that are secret: a pod carrying the owner
// name in plain text is clean, and one carrying a launcher token or a hosts.yml is a leak.
//
// The script is driven as the stage drives it: a stub kubectl answers the Secret list from a
// fixture and holds the watch open, the pod watch is a fixture file, SIGTERM makes it judge.
const script = join(import.meta.dir, "secret-leaks.ts");
const dir = mkdtempSync(join(tmpdir(), "secret-leaks-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
// kubectl: `get --raw <secrets?labelSelector=…>` prints the fixture listing; the watch
// (`&watch=1`) marks that it opened (WATCHED), prints one bookmark and stays open until killed,
// as the real one does.
writeFileSync(
  join(bin, "kubectl"),
  `#!/bin/bash
path=\${@: -1}
case "$path" in
  *watch=1*) : >"$WATCHED"; printf '%s\\n' '{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"2"}}}'; exec sleep 300 ;;
  *) cat "$SECRETS" ;;
esac
`,
  { mode: 0o755 }
);

const roles = ["architect", "planner", "implementer", "tester", "reviewer", "merger"];
const owner = "sjawhar";
const provisionToken = "pt_".padEnd(43, "p");
const launcherToken = (role: string) => `lt_${role}_${"k".repeat(40)}`;
const hostsYml = (role: string) =>
  `github.com:\n    user: x-access-token\n    oauth_token: ghs_${role}${"A".repeat(30)}\n    git_protocol: https\n`;
const configYml = "git_protocol: https\nprompt: disabled\n";
const b64 = (value: string) => Buffer.from(value, "utf8").toString("base64");

// The Secrets of one Sandbox pod as the daemon writes them: the pod's own `<pod>-boot` (podkind.go:
// the provisioning and repository-cache tokens) and each role's `<pod>-<role>-boot` (relaunch.go
// writeLauncherSecrets: the launcher token, the default owner, each owner's hosts.yml and config.yml).
function secretListing(pod: string) {
  const items = [
    {
      metadata: { name: `${pod}-boot`, resourceVersion: "1" },
      data: {
        LEGION_PROVISION_TOKEN: b64(provisionToken),
        LEGION_REPOSITORY_CACHE_TOKEN: b64("ct_".padEnd(43, "q")),
      },
    },
    ...roles.map((role) => ({
      metadata: { name: `${pod}-${role}-boot`, resourceVersion: "1" },
      data: {
        LAUNCHER_TOKEN: b64(launcherToken(role)),
        "github-default": b64(owner),
        [`github-hosts-${owner}`]: b64(hostsYml(role)),
        [`github-config-${owner}`]: b64(configYml),
      },
    })),
  ];
  return JSON.stringify({ metadata: { resourceVersion: "1" }, items });
}

// One ready issue pod as the pod watch records it: workspace-fetch and workspace-init carry the
// repository `<owner>/<repo>` and the cache URL naming the owner; its one `roles` container ready.
function podEvent(pod: string, uid: string, extraArgs: string[] = []) {
  return JSON.stringify({
    type: "MODIFIED",
    object: {
      kind: "Pod",
      metadata: {
        uid,
        name: pod,
        labels: { "legion.dev/project": "legsmoke", "legion.dev/tree": "LEGSMOKE-1" },
      },
      spec: {
        initContainers: [
          {
            name: "workspace-fetch",
            args: [
              "workspace-init",
              "fetch",
              "--repo",
              `${owner}/legion-smoke`,
              "--cache-url",
              `http://legion-legsmoke-cache-${owner}-legion-smoke-6687cbae.legion.svc:8418`,
            ],
          },
          {
            name: "workspace-init",
            args: ["workspace-init", "provision", "--repo", `${owner}/legion-smoke`, ...extraArgs],
          },
        ],
        containers: [
          {
            name: "roles",
            args: [
              "launcher",
              "--roles-dir",
              "/var/run/legion/roles",
              ...roles.flatMap((role) => ["--role", role]),
            ],
            env: [{ name: "POD_UID", value: "" }],
          },
        ],
      },
      status: { containerStatuses: [{ name: "roles", ready: true }] },
    },
  });
}

async function verdictFor(
  podWatchLine: string,
  secrets: string
): Promise<{ leaks: { pod: string; secret: string }[]; unseen: unknown[]; unreadable: number }> {
  const run = mkdtempSync(join(dir, "run."));
  const podWatch = join(run, "pod-watch.json");
  const secretsFile = join(run, "secrets.json");
  const verdict = join(run, "verdict.json");
  writeFileSync(podWatch, `${podWatchLine}\n`);
  writeFileSync(secretsFile, secrets);
  const watched = join(run, "watched");
  // The watch opens only once the list was read and remembered, so the stub's mark that it opened
  // is the signal that the judgement may be asked for.
  const opened = new Promise<void>((resolve) => {
    const watcher = watch(run, (_event, file) => {
      if (file === "watched") {
        watcher.close();
        resolve();
      }
    });
    if (existsSync(watched)) {
      watcher.close();
      resolve();
    }
  });
  const child = Bun.spawn(
    ["bun", script, "ctx", "legion", "legion.dev/project=legsmoke", podWatch, verdict],
    {
      env: {
        ...process.env,
        PATH: `${bin}:${process.env.PATH ?? ""}`,
        SECRETS: secretsFile,
        WATCHED: watched,
      },
      stdout: "ignore",
      stderr: "pipe",
    }
  );
  await opened;
  child.kill("SIGTERM");
  await child.exited;
  return JSON.parse(readFileSync(verdict, "utf8"));
}

describe("secret-leaks judges only the values that are secret", () => {
  const pod = "legion-legsmoke-legsmoke-1";
  const uid = "2b1c3c03-5708-431f-90d7-a9210d596d1f";

  test("a pod whose args carry the default owner's name, as every issue pod's workspace-fetch and workspace-init do, is no leak", async () => {
    const got = await verdictFor(podEvent(pod, uid), secretListing(pod));
    expect(got.unreadable).toBe(0);
    expect(got.unseen).toEqual([]);
    expect(
      got.leaks,
      "the owner name github-default holds is no credential; flagging it fails every run since #2008"
    ).toEqual([]);
  });

  // While the check waited for six role containers, a pod of one `roles` container was never
  // judged, so every leak in it passed.
  test("a pod whose one roles container is not yet ready is not judged; once ready, it is", async () => {
    const unready = JSON.parse(podEvent(pod, uid, ["--token", launcherToken("tester")]));
    unready.object.status.containerStatuses[0].ready = false;
    expect((await verdictFor(JSON.stringify(unready), secretListing(pod))).leaks).toEqual([]);
    const ready = await verdictFor(
      podEvent(pod, uid, ["--token", launcherToken("tester")]),
      secretListing(pod)
    );
    expect(ready.leaks.map((l) => l.secret)).toEqual([`${pod}-tester-boot`]);
  });

  test("negative control: a pod carrying a role's launcher token, its hosts.yml, or the pod's provisioning token in its args is a leak of that Secret", async () => {
    const provision = await verdictFor(
      podEvent(pod, uid, ["--token", provisionToken]),
      secretListing(pod)
    );
    expect(provision.leaks.map((l) => l.secret)).toContain(`${pod}-boot`);
    const token = await verdictFor(
      podEvent(pod, uid, ["--token", launcherToken("tester")]),
      secretListing(pod)
    );
    expect(token.leaks.map((l) => l.secret)).toContain(`${pod}-tester-boot`);
    const hosts = await verdictFor(
      podEvent(pod, uid, ["--hosts", hostsYml("merger")]),
      secretListing(pod)
    );
    expect(hosts.leaks.map((l) => l.secret)).toContain(`${pod}-merger-boot`);
  });
});
