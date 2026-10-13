import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { runJq } from "./run-jq.ts";

// Stage 4b's verdict on a deployment's worker pod block (deployment-pod-block): the issue pod the
// daemon launched from the block must carry its ServiceAccount, every volume as the block declares
// it, every mount in every role container, and every env name and value on a role's process.

const program = fileURLToPath(new URL("./deployment-pod-block-verdict.jq", import.meta.url));
const dir = mkdtempSync(join(tmpdir(), "deployment-pod-block-verdict-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

interface Verdict {
  process: { pid: number; comm: string } | null;
  missing: string[];
  wrong: string[];
}

// The block, in the shape Legion's loader reads `runtime.kubernetes.pod`.
interface TokenSource {
  service_account_token: { audience?: string; expiration_seconds?: number; path: string };
}
interface ObjectSource {
  config_map?: { name: string };
  secret?: { name: string };
}
interface BlockVolume {
  name: string;
  projected?: { sources: (TokenSource | ObjectSource)[] };
  config_map?: { name: string; items?: { key: string; path: string }[] };
  secret?: { name: string };
}
interface Block {
  service_account?: string;
  env?: Record<string, string>;
  volumes: BlockVolume[];
  volume_mounts: { volume: string; mount_path: string; sub_path?: string; read_only?: boolean }[];
}

// The pod, in the shape the API server returns.
interface PodToken {
  audience: string;
  expirationSeconds: number;
  path: string;
}
interface PodVolume {
  name: string;
  projected?: { sources: { serviceAccountToken: PodToken }[] };
  configMap?: { name: string; items?: { key: string; path: string }[] };
  secret?: { secretName: string };
  persistentVolumeClaim?: { claimName: string };
}
interface PodMount {
  name: string;
  mountPath: string;
  readOnly?: boolean;
  subPath?: string;
}
interface PodContainer {
  name: string;
  env: { name: string }[];
  volumeMounts: PodMount[];
}
interface Pod {
  metadata: { name: string };
  spec: {
    serviceAccountName: string;
    automountServiceAccountToken: boolean;
    initContainers: { name: string; volumeMounts: PodMount[] }[];
    containers: PodContainer[];
    volumes: PodVolume[];
  };
}

const kubeToken = (): PodToken => ({
  audience: "https://kubernetes.default.svc",
  expirationSeconds: 3600,
  path: "token",
});
const operatorToken = (): PodToken => ({
  audience: "operator",
  expirationSeconds: 3600,
  path: "token",
});

const block = (): Block => ({
  service_account: "legion-worker",
  env: {
    KUBECONFIG: "/var/run/legion/kubernetes/config/config",
    HAWK_API_URL: "https://hawk.internal.example",
  },
  volumes: [
    {
      name: "kubernetes-api-token",
      projected: {
        sources: [
          {
            service_account_token: {
              audience: "https://kubernetes.default.svc",
              expiration_seconds: 3600,
              path: "token",
            },
          },
        ],
      },
    },
    {
      name: "kubernetes-api-ca",
      config_map: { name: "kube-root-ca.crt", items: [{ key: "ca.crt", path: "ca.crt" }] },
    },
    { name: "operator-creds", secret: { name: "legion-operator-creds" } },
    {
      name: "operator-token",
      projected: { sources: [{ service_account_token: { audience: "operator", path: "token" } }] },
    },
  ],
  volume_mounts: [
    { volume: "kubernetes-api-token", mount_path: "/var/run/legion/kubernetes/token" },
    { volume: "kubernetes-api-ca", mount_path: "/var/run/legion/kubernetes/ca" },
    { volume: "operator-creds", mount_path: "/etc/legion-operator/creds", read_only: false },
    { volume: "operator-token", mount_path: "/var/run/operator" },
  ],
});

const roleMounts = (): PodMount[] => [
  { name: "issue", mountPath: "/legion" },
  { name: "kubernetes-api-token", mountPath: "/var/run/legion/kubernetes/token", readOnly: true },
  { name: "kubernetes-api-ca", mountPath: "/var/run/legion/kubernetes/ca", readOnly: true },
  { name: "operator-creds", mountPath: "/etc/legion-operator/creds" },
  { name: "operator-token", mountPath: "/var/run/operator", readOnly: true },
];

const pod = (): Pod => ({
  metadata: { name: "legion-legsmoke-legsmoke-1" },
  spec: {
    serviceAccountName: "legion-worker",
    automountServiceAccountToken: false,
    initContainers: [{ name: "workspace-init", volumeMounts: [] }],
    containers: ["architect", "planner"].map((name) => ({
      name,
      env: [{ name: "POD_UID" }],
      volumeMounts: roleMounts(),
    })),
    volumes: [
      { name: "issue", persistentVolumeClaim: { claimName: "x" } },
      {
        name: "kubernetes-api-token",
        projected: { sources: [{ serviceAccountToken: kubeToken() }] },
      },
      {
        name: "kubernetes-api-ca",
        configMap: { name: "kube-root-ca.crt", items: [{ key: "ca.crt", path: "ca.crt" }] },
      },
      { name: "operator-creds", secret: { secretName: "legion-operator-creds" } },
      {
        name: "operator-token",
        projected: { sources: [{ serviceAccountToken: operatorToken() }] },
      },
    ],
  },
});

const fullEnv =
  "KUBECONFIG=/var/run/legion/kubernetes/config/config\nHAWK_API_URL=https://hawk.internal.example\n";
const processes = (env = fullEnv) => `#1 legion\n#10 legion\n${env}#24 sh\n`;

let n = 0;
const verdict = (thePod: Pod, theBlock: Block = block(), procs = processes()): Verdict => {
  n += 1;
  const blockFile = join(dir, `block-${n}.json`);
  const procFile = join(dir, `processes-${n}.txt`);
  writeFileSync(blockFile, JSON.stringify(theBlock));
  writeFileSync(procFile, procs);
  const out = runJq(
    ["-c", "--slurpfile", "block", blockFile, "--rawfile", "processes", procFile, "-f", program],
    JSON.stringify(thePod)
  );
  const parsed: Verdict = JSON.parse(out);
  return parsed;
};

describe("deployment-pod-block verdict", () => {
  test("a pod carrying the whole block passes, and names the process holding its env", () => {
    expect(verdict(pod())).toEqual({
      process: { pid: 10, comm: "legion" },
      missing: [],
      wrong: [],
    });
  });

  test("a block with no env asks for no process", () => {
    const b = block();
    delete b.env;
    expect(verdict(pod(), b, "#1 legion\n")).toEqual({ process: null, missing: [], wrong: [] });
  });

  test("another ServiceAccount is wrong", () => {
    const p = pod();
    p.spec.serviceAccountName = "default";
    expect(verdict(p).wrong).toEqual(["the pod runs as ServiceAccount default, not legion-worker"]);
  });

  test("a volume the pod lacks is missing", () => {
    const p = pod();
    p.spec.volumes = p.spec.volumes.filter((v) => v.name !== "kubernetes-api-ca");
    expect(verdict(p).missing).toEqual(["volume kubernetes-api-ca"]);
  });

  test("a projected token for another audience, expiry or path is wrong", () => {
    const p = pod();
    p.spec.volumes[1] = {
      name: "kubernetes-api-token",
      projected: {
        sources: [
          {
            serviceAccountToken: {
              audience: "sts.amazonaws.com",
              expirationSeconds: 600,
              path: "other",
            },
          },
        ],
      },
    };
    expect(verdict(p).wrong).toEqual([
      "volume kubernetes-api-token source 0 token audience sts.amazonaws.com is not https://kubernetes.default.svc",
      "volume kubernetes-api-token source 0 token expires in 600 s, not 3600",
      "volume kubernetes-api-token source 0 token path other is not token",
    ]);
  });

  test("a token with no expiration_seconds is held to the API server's 3600 s", () => {
    const p = pod();
    p.spec.volumes[4] = {
      name: "operator-token",
      projected: {
        sources: [{ serviceAccountToken: { ...operatorToken(), expirationSeconds: 7200 } }],
      },
    };
    expect(verdict(p).wrong).toEqual([
      "volume operator-token source 0 token expires in 7200 s, not 3600",
    ]);
  });

  test("a volume of another kind or naming another object is wrong", () => {
    const p = pod();
    p.spec.volumes[2] = { name: "kubernetes-api-ca", secret: { secretName: "kube-root-ca.crt" } };
    p.spec.volumes[3] = { name: "operator-creds", secret: { secretName: "someone-else" } };
    expect(verdict(p).wrong).toEqual([
      "volume kubernetes-api-ca is secret, not config_map",
      "volume operator-creds is Secret someone-else, not legion-operator-creds",
    ]);
  });

  test("a projected source the pod lacks is missing", () => {
    const b = block();
    b.volumes[0] = {
      name: "kubernetes-api-token",
      projected: {
        sources: [
          {
            service_account_token: {
              audience: "https://kubernetes.default.svc",
              expiration_seconds: 3600,
              path: "token",
            },
          },
          { config_map: { name: "extra" } },
        ],
      },
    };
    expect(verdict(pod(), b).missing).toEqual([
      "volume kubernetes-api-token source 1 (config_map)",
    ]);
  });

  test("a mount missing from one role container is missing there alone", () => {
    const p = pod();
    p.spec.containers[1].volumeMounts = roleMounts().filter(
      (m) => m.name !== "kubernetes-api-token"
    );
    expect(verdict(p).missing).toEqual([
      "mount kubernetes-api-token at /var/run/legion/kubernetes/token in container planner",
    ]);
  });

  test("an init container is not held to the block's mounts", () => {
    const p = pod();
    expect(p.spec.initContainers[0].volumeMounts).toEqual([]);
    expect(verdict(p).missing).toEqual([]);
  });

  test("a mount's sub_path or read_only that differs is wrong", () => {
    const p = pod();
    p.spec.containers[0].volumeMounts[2] = {
      name: "kubernetes-api-ca",
      mountPath: "/var/run/legion/kubernetes/ca",
      readOnly: false,
      subPath: "ca.crt",
    };
    expect(verdict(p).wrong).toEqual([
      "mount kubernetes-api-ca at /var/run/legion/kubernetes/ca in container architect has sub_path ca.crt",
      "mount kubernetes-api-ca at /var/run/legion/kubernetes/ca in container architect has read_only false",
    ]);
  });

  test("a writable mount the block asks for is held to read_only false", () => {
    const p = pod();
    p.spec.containers[0].volumeMounts[3] = {
      name: "operator-creds",
      mountPath: "/etc/legion-operator/creds",
      readOnly: true,
    };
    expect(verdict(p).wrong).toEqual([
      "mount operator-creds at /etc/legion-operator/creds in container architect has read_only true",
    ]);
  });

  test("an env name no process holds is missing, and the verdict never prints a value", () => {
    const v = verdict(
      pod(),
      block(),
      processes("KUBECONFIG=/var/run/legion/kubernetes/config/config\n")
    );
    expect(v.missing).toEqual(["env HAWK_API_URL on process 10"]);
    const none = verdict(pod(), block(), "#1 legion\n");
    expect(none.process).toBeNull();
    expect(none.missing).toEqual([
      "env KUBECONFIG on any process of the architect container",
      "env HAWK_API_URL on any process of the architect container",
    ]);
  });

  test("an env value that differs is wrong, named and not printed", () => {
    const v = verdict(
      pod(),
      block(),
      processes("KUBECONFIG=/elsewhere\nHAWK_API_URL=https://hawk.internal.example\n")
    );
    expect(v.wrong).toEqual(["env KUBECONFIG on process 10 differs from the block"]);
    expect(JSON.stringify(v)).not.toContain("/elsewhere");
  });

  test("the process holding the most of the block's env names is the one judged", () => {
    const procs = `#1 legion\n#5 sh\nHAWK_API_URL=https://hawk.internal.example\n#10 legion\n${fullEnv}`;
    expect(verdict(pod(), block(), procs)).toEqual({
      process: { pid: 10, comm: "legion" },
      missing: [],
      wrong: [],
    });
  });

  test("a value holding an equals sign is read whole", () => {
    const b = block();
    b.env = { QUERY: "a=b=c" };
    expect(verdict(pod(), b, "#7 omp\nQUERY=a=b=c\n")).toEqual({
      process: { pid: 7, comm: "omp" },
      missing: [],
      wrong: [],
    });
  });
});
