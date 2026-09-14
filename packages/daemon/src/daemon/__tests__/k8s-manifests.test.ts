import { describe, expect, it } from "bun:test";
import { DEFAULT_KUBERNETES_RESOURCES } from "../config";
import {
  buildPodManifest,
  buildPvcManifest,
  buildSecretManifest,
  LABEL_GENERATION,
  LABEL_ISSUE,
  LABEL_PROJECT,
  LABEL_ROLE,
  LABEL_TREE,
  type PodManifestInput,
  podLabels,
  podName,
  podSelector,
  podWorkspaceDir,
  pvcName,
} from "../k8s-manifests";

const DIGEST = `ghcr.io/x@sha256:${"a".repeat(64)}`;

function basePodInput(overrides: Partial<PodManifestInput> = {}): PodManifestInput {
  return {
    project: "acme7",
    tree: "LEGION-24",
    issue: "LEGION-24",
    role: "planner",
    generation: 1,
    namespace: "legion",
    image: DIGEST,
    pvcName: "legion-legion-24",
    podName: "legion-legion-24-planner-g1",
    secretName: "legion-legion-24-planner-g1",
    secretKeys: ["LEGION_BOOT_TOKEN"],
    resources: DEFAULT_KUBERNETES_RESOURCES.small,
    env: { A: "1" },
    workspaceDir: "/legion/workspaces/acme/widgets/legion-24",
    repo: "acme/widgets",
    shimEndpoint: "tcp://172.18.0.1:19371",
    ompArgv: [
      "omp",
      "--resume=/home/legion/.omp/profiles/legion/agent/sessions/s.jsonl",
      "--mode",
      "rpc",
      "--append-system-prompt",
      "ROLE",
      "--append-system-prompt",
      "ADDR",
      "--append-system-prompt",
      "INSTR",
    ],
    terminationGracePeriodSeconds: 10,
    workspaceInitLockWaitSeconds: 480,
    resumeSessionFile: "/home/legion/.omp/profiles/legion/agent/sessions/s.jsonl",
    ...overrides,
  };
}

describe("podName / pvcName", () => {
  it("builds a pod name from issue, role, and generation", () => {
    expect(podName("LEGION-24", "planner", 2)).toBe("legion-legion-24-planner-g2");
  });

  it("truncates and hashes an issue key that slugs past 38 characters", () => {
    const issue = "x".repeat(70);
    const name = podName(issue, "planner", 1);
    expect(name.length).toBeLessThanOrEqual(63);
    expect(name.endsWith("-planner-g1")).toBe(true);
    expect(name).toMatch(/-[0-9a-f]{8}-planner-g1$/);
  });

  it("builds a pvc name from the tree key", () => {
    expect(pvcName("LEGION-24")).toBe("legion-legion-24");
  });
});

describe("podLabels / podSelector", () => {
  it("builds the exact label set", () => {
    expect(
      podLabels({
        project: "acme7",
        tree: "LEGION-24",
        issue: "LEGION-24",
        role: "planner",
        generation: 1,
      })
    ).toEqual({
      [LABEL_PROJECT]: "acme7",
      [LABEL_TREE]: "LEGION-24",
      [LABEL_ISSUE]: "LEGION-24",
      [LABEL_ROLE]: "planner",
      [LABEL_GENERATION]: "1",
    });
  });

  it("joins labels in the given key order", () => {
    expect(podSelector({ b: "2", a: "1" })).toBe("b=2,a=1");
  });
});

describe("podWorkspaceDir", () => {
  it("builds the tree-mounted workspace path", () => {
    expect(podWorkspaceDir("acme/widgets", "LEGION-24")).toBe(
      "/legion/workspaces/acme/widgets/legion-24"
    );
  });
});

describe("buildPodManifest", () => {
  it("matches the exact acceptance snapshot", () => {
    expect(buildPodManifest(basePodInput())).toEqual({
      apiVersion: "v1",
      kind: "Pod",
      metadata: {
        name: "legion-legion-24-planner-g1",
        namespace: "legion",
        labels: {
          "legion.dev/project": "acme7",
          "legion.dev/tree": "LEGION-24",
          "legion.dev/issue": "LEGION-24",
          "legion.dev/role": "planner",
          "legion.dev/generation": "1",
        },
      },
      spec: {
        restartPolicy: "Never",
        terminationGracePeriodSeconds: 10,
        automountServiceAccountToken: false,
        enableServiceLinks: false,
        securityContext: { runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000 },
        affinity: {
          podAffinity: {
            requiredDuringSchedulingIgnoredDuringExecution: [
              {
                labelSelector: { matchLabels: { "legion.dev/tree": "LEGION-24" } },
                topologyKey: "kubernetes.io/hostname",
              },
            ],
          },
        },
        volumes: [
          { name: "tree", persistentVolumeClaim: { claimName: "legion-legion-24" } },
          {
            name: "providers",
            secret: { secretName: "legion-acme7-providers", defaultMode: 0o440 },
          },
          {
            name: "boot",
            secret: {
              secretName: "legion-legion-24-planner-g1",
              defaultMode: 0o440,
              items: [{ key: "LEGION_BOOT_TOKEN", path: "LEGION_BOOT_TOKEN" }],
            },
          },
          {
            name: "provision",
            secret: {
              secretName: "legion-legion-24-planner-g1",
              defaultMode: 0o440,
              items: [{ key: "LEGION_PROVISION_TOKEN", path: "LEGION_PROVISION_TOKEN" }],
            },
          },
          { name: "grant", emptyDir: { medium: "Memory", sizeLimit: "1Mi" } },
        ],
        initContainers: [
          {
            name: "workspace-init",
            image: DIGEST,
            command: [
              "/opt/legion/bin/legion",
              "workspace-init",
              "--issue",
              "LEGION-24",
              "--repo",
              "acme/widgets",
              "--root",
              "/legion",
              "--credential-helper",
              "!/opt/legion/bin/legion credential",
            ],
            env: [
              {
                name: "LEGION_PROVISION_TOKEN_FILE",
                value: "/var/run/legion/provision/LEGION_PROVISION_TOKEN",
              },
              { name: "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS", value: "480" },
              { name: "LEGION_RESUME_SESSION_FILE", value: "/legion/sessions/s.jsonl" },
            ],
            workingDir: "/legion",
            volumeMounts: [
              { name: "tree", mountPath: "/legion" },
              { name: "provision", mountPath: "/var/run/legion/provision", readOnly: true },
            ],
            resources: {
              requests: { cpu: "500m", memory: "1Gi", "ephemeral-storage": "2Gi" },
              limits: { cpu: "2", memory: "3Gi", "ephemeral-storage": "8Gi" },
            },
          },
        ],
        containers: [
          {
            name: "worker",
            image: DIGEST,
            command: [
              "/opt/legion/bin/legion",
              "worker-shim",
              "--connect",
              "tcp://172.18.0.1:19371",
              "--boot-token-file",
              "/var/run/legion/boot/LEGION_BOOT_TOKEN",
              "--provider-env-dir",
              "/var/run/legion/providers",
              "--",
              "omp",
              "--resume=/home/legion/.omp/profiles/legion/agent/sessions/s.jsonl",
              "--mode",
              "rpc",
              "--append-system-prompt",
              "ROLE",
              "--append-system-prompt",
              "ADDR",
              "--append-system-prompt",
              "INSTR",
            ],
            env: [
              { name: "A", value: "1" },
              { name: "LEGION_TERMINATION_GRACE_SECONDS", value: "10" },
            ],
            workingDir: "/legion/workspaces/acme/widgets/legion-24",
            volumeMounts: [
              { name: "tree", mountPath: "/legion" },
              {
                name: "tree",
                mountPath: "/home/legion/.omp/profiles/legion/agent/sessions",
                subPath: "sessions",
              },
              { name: "providers", mountPath: "/var/run/legion/providers", readOnly: true },
              { name: "boot", mountPath: "/var/run/legion/boot", readOnly: true },
              { name: "grant", mountPath: "/var/run/legion/grant" },
            ],
            resources: {
              requests: { cpu: "500m", memory: "1Gi", "ephemeral-storage": "2Gi" },
              limits: { cpu: "2", memory: "3Gi", "ephemeral-storage": "8Gi" },
            },
          },
        ],
      },
    });
  });

  it("throws naming the oversized argv element and MAX_ARG_STRLEN", () => {
    expect(() =>
      buildPodManifest(
        basePodInput({
          ompArgv: ["omp", "--append-system-prompt", "x".repeat(200_000)],
        })
      )
    ).toThrow(
      "omp argv: the value of --append-system-prompt is 200000 bytes; a single argv string may not exceed 131072 (Linux MAX_ARG_STRLEN)"
    );
    expect(() =>
      buildPodManifest(basePodInput({ ompArgv: ["omp", `--resume=${"y".repeat(200_000)}`] }))
    ).toThrow(/omp argv: argv element #1 \("--resume=yyyy.*…\) is 200009 bytes/);
  });

  it("projects one boot-volume item per secret key, in the given order", () => {
    const manifest = buildPodManifest(
      basePodInput({ secretKeys: ["LEGION_BOOT_TOKEN", "ENVOY_TOKEN"] })
    );
    const volumes = manifest.spec.volumes;
    expect(volumes).toContainEqual({
      name: "boot",
      secret: {
        secretName: "legion-legion-24-planner-g1",
        defaultMode: 0o440,
        items: [
          { key: "LEGION_BOOT_TOKEN", path: "LEGION_BOOT_TOKEN" },
          { key: "ENVOY_TOKEN", path: "ENVOY_TOKEN" },
        ],
      },
    });
  });
});

describe("buildPvcManifest", () => {
  it("includes storageClassName when given", () => {
    const pvc = buildPvcManifest({
      name: "legion-legion-24",
      project: "acme7",
      tree: "LEGION-24",
      storageClass: "fast-ssd",
      size: "20Gi",
    });
    expect(pvc.spec.storageClassName).toBe("fast-ssd");
  });

  it("omits storageClassName entirely when not given", () => {
    const pvc = buildPvcManifest({
      name: "legion-legion-24",
      project: "acme7",
      tree: "LEGION-24",
      size: "20Gi",
    });
    expect("storageClassName" in pvc.spec).toBe(false);
    expect(pvc.spec).toEqual({
      accessModes: ["ReadWriteOnce"],
      resources: { requests: { storage: "20Gi" } },
    });
  });
});

describe("buildSecretManifest", () => {
  it("uses stringData and type Opaque", () => {
    const secret = buildSecretManifest({
      name: "legion-legion-24-planner-g1",
      labels: { [LABEL_PROJECT]: "acme7" },
      stringData: { LEGION_BOOT_TOKEN: "boot-token-value" },
    });
    expect(secret.type).toBe("Opaque");
    expect(secret.stringData).toEqual({ LEGION_BOOT_TOKEN: "boot-token-value" });
  });
});
