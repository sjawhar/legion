# Pulumi IaC for Envoy Deployment — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Docker Compose + shell wrappers with a single `pulumi up` that deploys NATS peers, listeners, and receivers across 4 machines over Tailscale SSH.

**Architecture:** Single Pulumi stack with one `docker.Provider` per machine. Pre-built multi-arch images from public GHCR. NATS config rendered in TypeScript and injected via container `uploads`. Rolling per-machine migration preserving existing NATS data volumes. All containers use `deleteBeforeReplace: true`.

**Tech Stack:** TypeScript, `@pulumi/pulumi`, `@pulumi/docker` 4.11.0 (pinned exact), Bun test runner

**Dependency:** Issue #206 (KV non-fatal) must ship first. Do not merge this until #206 is on main.

---

## Assumptions

1. SSH user@host values from architect spec: `ubuntu@sami-agents-mx`, `sami@sami`, `claude@sami-claude`, `ghost-wispr@ghost-wispr`
2. ghost-wispr is listener-only (no NATS peer) — 3-node NATS cluster: sami-agents-mx, sami, sami-claude
3. GHCR images are public — no `registryAuth` needed for pull on remote hosts
4. Pulumi state backend: default (Pulumi Cloud or local file — operator chooses at `pulumi login`)
5. All machines reachable over Tailscale MagicDNS hostnames via SSH
6. ghost-wispr architecture TBD — multi-arch images (amd64+arm64) handle either case
7. Existing NATS data volume is named `nats_nats_data` (Docker Compose project-prefixed from `compose/nats/peer.compose.yml`). **Verify per-machine during preflight.**

---

## File Structure

```
packages/envoy/infra/          # NEW — Pulumi project
├── Pulumi.yaml                # Project definition
├── Pulumi.prod.yaml           # Stack config + encrypted secrets
├── package.json               # Pinned dependencies
├── tsconfig.json              # TypeScript config (strict, matches repo)
├── index.ts                   # Entry point — reads config, assembles resources
├── machines.ts                # MachineConfig type + docker.Provider factory
├── nats.ts                    # NATS config rendering, route computation, peer container
├── services.ts                # NATS URL computation, listener/GitHub/Slack containers
├── images.ts                  # Conditional RemoteImage resources per machine
└── __tests__/
    ├── nats.test.ts           # Tests for renderNatsConf, computeNatsRoutes
    └── services.test.ts       # Tests for computeNatsUrls

packages/envoy/scripts/
└── build-push-images.sh       # NEW — Multi-arch image build + push to GHCR
```

---

### Task 1: Scaffold `packages/envoy/infra` package — Independent

**Files:**
- Create: `packages/envoy/infra/package.json`
- Create: `packages/envoy/infra/tsconfig.json`
- Create: `packages/envoy/infra/Pulumi.yaml`

- [ ] **Step 1: Create package.json with pinned dependencies**

```json
{
  "name": "envoy-infra",
  "version": "0.0.1",
  "private": true,
  "main": "index.ts",
  "dependencies": {
    "@pulumi/pulumi": "^3.0.0",
    "@pulumi/docker": "4.11.0"
  },
  "devDependencies": {
    "@types/bun": "latest"
  }
}
```

> **Critical:** `@pulumi/docker` pinned to exact `4.11.0` — SSH transport has had regressions in this provider. Do NOT use `^4.11.0` or `~4.11.0`.

- [ ] **Step 2: Create tsconfig.json**

```json
{
  "compilerOptions": {
    "target": "ESNext",
    "module": "ESNext",
    "lib": ["ESNext"],
    "moduleResolution": "bundler",
    "strict": true,
    "esModuleInterop": true,
    "skipLibCheck": true,
    "forceConsistentCasingInFileNames": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "outDir": "./dist",
    "rootDir": "."
  },
  "include": ["./**/*.ts"],
  "exclude": ["node_modules", "dist"]
}
```

- [ ] **Step 3: Create Pulumi.yaml**

```yaml
name: envoy
runtime: nodejs
description: Envoy fleet deployment via Docker over Tailscale SSH
```

- [ ] **Step 4: Install dependencies**

Run: `cd packages/envoy/infra && bun install`
Expected: `node_modules/` created, lockfile written, no errors.

- [ ] **Step 5: Commit scaffold**

```bash
jj describe -m "feat(envoy): scaffold Pulumi infra package with pinned @pulumi/docker 4.11.0"
jj new
```

---

### Task 2: Machine config types and provider factory — Depends on: Task 1

**Files:**
- Create: `packages/envoy/infra/machines.ts`

- [ ] **Step 1: Create machines.ts with types and provider factory**

```typescript
import * as docker from "@pulumi/docker";

export interface NatsConfig {
  serverName: string;
}

export interface ListenerConfig {
  registryDir: string;
}

export interface ReceiverConfig {
  github?: boolean;
  slack?: boolean;
}

export interface MachineConfig {
  name: string;
  sshHost: string;
  machineId: string;
  nats?: NatsConfig;
  listener: ListenerConfig;
  receivers?: ReceiverConfig;
}

export function createProvider(machine: MachineConfig): docker.Provider {
  return new docker.Provider(`docker-${machine.name}`, {
    host: machine.sshHost,
  });
}
```

- [ ] **Step 2: Verify types compile**

Run: `cd packages/envoy/infra && bunx tsc --noEmit`
Expected: Clean exit, no errors.

- [ ] **Step 3: Commit**

```bash
jj describe -m "feat(envoy): add MachineConfig types and Docker provider factory"
jj new
```

---

### Task 3: NATS module — config helpers, peer container, tests — Depends on: Task 1, Task 2

**Files:**
- Create: `packages/envoy/infra/nats.ts`
- Create: `packages/envoy/infra/__tests__/nats.test.ts`

- [ ] **Step 1: Write failing tests for NATS config rendering and route computation**

Create `packages/envoy/infra/__tests__/nats.test.ts`:

```typescript
import { describe, expect, test } from "bun:test";
import { computeNatsRoutes, renderNatsConf } from "../nats";

describe("computeNatsRoutes", () => {
  const machines = [
    { name: "sami-agents-mx", nats: true },
    { name: "sami", nats: true },
    { name: "sami-claude", nats: true },
    { name: "ghost-wispr", nats: false },
  ];

  test("returns routes to all OTHER peers", () => {
    const routes = computeNatsRoutes("sami-agents-mx", machines);
    expect(routes).toEqual([
      "nats://sami:6222",
      "nats://sami-claude:6222",
    ]);
  });

  test("excludes non-NATS machines", () => {
    const routes = computeNatsRoutes("sami", machines);
    expect(routes).toEqual([
      "nats://sami-agents-mx:6222",
      "nats://sami-claude:6222",
    ]);
    expect(routes.some((r) => r.includes("ghost-wispr"))).toBe(false);
  });

  test("returns all peer routes for a non-peer machine", () => {
    const routes = computeNatsRoutes("ghost-wispr", machines);
    expect(routes).toEqual([
      "nats://sami-agents-mx:6222",
      "nats://sami:6222",
      "nats://sami-claude:6222",
    ]);
  });
});

describe("renderNatsConf", () => {
  test("renders valid nats.conf matching deploy/scripts/render-nats-peer.sh output", () => {
    const conf = renderNatsConf("sami-agents-mx", [
      "nats://sami:6222",
      "nats://sami-claude:6222",
    ]);

    expect(conf).toContain("server_name=sami-agents-mx");
    expect(conf).toContain("listen=0.0.0.0:4222");
    expect(conf).toContain("store_dir=/data");
    expect(conf).toContain("name: envoy");
    expect(conf).toContain("listen: 0.0.0.0:6222");
    expect(conf).toContain("nats://sami:6222");
    expect(conf).toContain("nats://sami-claude:6222");
    expect(conf).not.toContain("sami-agents-mx:6222");
  });

  test("handles single-route cluster", () => {
    const conf = renderNatsConf("node-a", ["nats://node-b:6222"]);
    expect(conf).toContain("server_name=node-a");
    expect(conf).toContain("nats://node-b:6222");
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd packages/envoy/infra && bun test __tests__/nats.test.ts`
Expected: FAIL — `Cannot find module "../nats"`

- [ ] **Step 3: Create nats.ts with helpers and container resource**

```typescript
import * as docker from "@pulumi/docker";
import type { MachineConfig } from "./machines";

// --- Pure helpers (exported for testing) ---

interface PeerInfo {
  name: string;
  nats: boolean;
}

/**
 * Compute NATS cluster routes for a given machine.
 * Returns routes to all OTHER machines that run NATS peers.
 */
export function computeNatsRoutes(
  machineName: string,
  machines: PeerInfo[],
): string[] {
  return machines
    .filter((m) => m.nats && m.name !== machineName)
    .map((m) => `nats://${m.name}:6222`);
}

/**
 * Render nats.conf content matching the format produced by
 * deploy/scripts/render-nats-peer.sh.
 */
export function renderNatsConf(
  serverName: string,
  routes: string[],
): string {
  const routeLines = routes.map((r) => `    ${r}`).join("\n");
  return `server_name=${serverName}
listen=0.0.0.0:4222

jetstream {
  store_dir=/data
}

cluster {
  name: envoy
  listen: 0.0.0.0:6222
  routes: [
${routeLines}
  ]
}
`;
}

// --- Pulumi resources ---

/**
 * Create NATS peer resources on a machine: named volume + container.
 * Only called for machines with nats config.
 * Returns the container resource for dependency wiring.
 *
 * Volume name matches the existing Docker Compose-generated volume
 * (project "nats" + volume "nats_data" = "nats_nats_data").
 * Verify actual volume name per-machine during preflight.
 */
export function createNatsPeer(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  natsImage: docker.RemoteImage,
): docker.Container {
  const peerInfo = allMachines.map((m) => ({
    name: m.name,
    nats: !!m.nats,
  }));

  const routes = computeNatsRoutes(machine.name, peerInfo);
  const conf = renderNatsConf(machine.nats!.serverName, routes);

  // Named volume — matches existing compose-generated "nats_nats_data"
  // for zero-copy migration. docker compose down (without -v) preserves it.
  const volume = new docker.Volume(
    `nats-data-${machine.name}`,
    {
      name: "nats_nats_data",
    },
    { provider },
  );

  const container = new docker.Container(
    `nats-${machine.name}`,
    {
      name: "envoy-nats",
      image: natsImage.imageId,
      restart: "unless-stopped",
      ports: [
        { internal: 4222, external: 4222 },
        { internal: 6222, external: 6222 },
        { internal: 8222, external: 8222 },
      ],
      uploads: [
        {
          content: conf,
          file: "/etc/nats/nats.conf",
        },
      ],
      volumes: [
        {
          volumeName: volume.name,
          containerPath: "/data",
        },
      ],
      command: ["-c", "/etc/nats/nats.conf", "-m", "8222"],
      healthcheck: {
        tests: [
          "CMD",
          "wget",
          "-q",
          "--spider",
          "http://127.0.0.1:8222/healthz",
        ],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "5s",
      },
      wait: true,
      waitTimeout: 30,
    },
    { provider, deleteBeforeReplace: true },
  );

  return container;
}
```

> **Critical decisions:**
> - `deleteBeforeReplace: true` — NATS uses bridge networking with port mappings. Default create-before-delete causes port conflicts.
> - Volume name `"nats_nats_data"` — Docker Compose prefixes volume names with the project name. The compose file at `compose/nats/peer.compose.yml` creates project `nats` + volume `nats_data` = `nats_nats_data`. Verify per-machine during preflight.
> - Healthcheck uses `wget` (not `curl`) — the `nats:2.11-alpine` image includes `wget` via BusyBox but does NOT include `curl`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd packages/envoy/infra && bun test __tests__/nats.test.ts`
Expected: All tests PASS.

- [ ] **Step 5: Verify types compile**

Run: `cd packages/envoy/infra && bunx tsc --noEmit`
Expected: Clean exit, no errors.

- [ ] **Step 6: Commit**

```bash
jj describe -m "feat(envoy): add NATS module with config rendering, route computation, and peer container"
jj new
```

---

### Task 4: Images module — conditional per-machine pulls — Depends on: Task 2

**Files:**
- Create: `packages/envoy/infra/images.ts`

- [ ] **Step 1: Create images.ts with conditional image pulling**

```typescript
import * as docker from "@pulumi/docker";
import type { MachineConfig } from "./machines";

export interface MachineImages {
  nats?: docker.RemoteImage;
  listener: docker.RemoteImage;
  github?: docker.RemoteImage;
  slack?: docker.RemoteImage;
}

/**
 * Pull only the images a machine actually needs.
 * - All machines: listener
 * - NATS machines only: nats
 * - Receiver machines only: github, slack (as configured)
 *
 * Uses keepLocally: true to avoid deleting images on `pulumi destroy`.
 */
export function pullImages(
  provider: docker.Provider,
  machine: MachineConfig,
  registry: string,
  imageTag: string,
  natsImage: string,
): MachineImages {
  const result: MachineImages = {
    listener: new docker.RemoteImage(
      `listener-image-${machine.name}`,
      {
        name: `${registry}/envoy-listener:${imageTag}`,
        keepLocally: true,
      },
      { provider },
    ),
  };

  if (machine.nats) {
    result.nats = new docker.RemoteImage(
      `nats-image-${machine.name}`,
      {
        name: natsImage,
        keepLocally: true,
      },
      { provider },
    );
  }

  if (machine.receivers?.github) {
    result.github = new docker.RemoteImage(
      `github-image-${machine.name}`,
      {
        name: `${registry}/envoy-github:${imageTag}`,
        keepLocally: true,
      },
      { provider },
    );
  }

  if (machine.receivers?.slack) {
    result.slack = new docker.RemoteImage(
      `slack-image-${machine.name}`,
      {
        name: `${registry}/envoy-slack:${imageTag}`,
        keepLocally: true,
      },
      { provider },
    );
  }

  return result;
}
```

- [ ] **Step 2: Verify types compile**

Run: `cd packages/envoy/infra && bunx tsc --noEmit`
Expected: Clean exit, no errors.

- [ ] **Step 3: Commit**

```bash
jj describe -m "feat(envoy): add conditional image pulling module"
jj new
```

---

### Task 5: Services module — NATS URL helper, listener/receiver containers, tests — Depends on: Task 2, Task 4

**Files:**
- Create: `packages/envoy/infra/services.ts`
- Create: `packages/envoy/infra/__tests__/services.test.ts`

- [ ] **Step 1: Write failing tests for NATS URL computation**

Create `packages/envoy/infra/__tests__/services.test.ts`:

```typescript
import { describe, expect, test } from "bun:test";
import { computeNatsUrls } from "../services";

describe("computeNatsUrls", () => {
  const machines = [
    { name: "sami-agents-mx", nats: true },
    { name: "sami", nats: true },
    { name: "sami-claude", nats: true },
    { name: "ghost-wispr", nats: false },
  ];

  test("machine with local NATS peer gets 127.0.0.1 first, then remote peers", () => {
    const urls = computeNatsUrls("sami-agents-mx", true, machines);
    expect(urls).toBe(
      "nats://127.0.0.1:4222,nats://sami:4222,nats://sami-claude:4222",
    );
  });

  test("machine without local NATS peer gets all remote peers", () => {
    const urls = computeNatsUrls("ghost-wispr", false, machines);
    expect(urls).toBe(
      "nats://sami-agents-mx:4222,nats://sami:4222,nats://sami-claude:4222",
    );
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd packages/envoy/infra && bun test __tests__/services.test.ts`
Expected: FAIL — `Cannot find module "../services"`

- [ ] **Step 3: Create services.ts with NATS URL helper and container factories**

```typescript
import * as pulumi from "@pulumi/pulumi";
import * as docker from "@pulumi/docker";
import type { MachineConfig } from "./machines";
import type { MachineImages } from "./images";

// --- Pure helper (exported for testing) ---

interface PeerInfo {
  name: string;
  nats: boolean;
}

/**
 * Compute the NATS_URLS connection string for a machine.
 * Machines with a local NATS peer get 127.0.0.1 first, then all other peers.
 * Machines without a local peer get all peer URLs.
 */
export function computeNatsUrls(
  machineName: string,
  hasLocalNats: boolean,
  machines: PeerInfo[],
): string {
  const remotePeers = machines
    .filter((m) => m.nats && m.name !== machineName)
    .map((m) => `nats://${m.name}:4222`);

  if (hasLocalNats) {
    return ["nats://127.0.0.1:4222", ...remotePeers].join(",");
  }
  return remotePeers.join(",");
}

// --- Pulumi resources ---

interface ServiceSecrets {
  githubWebhookSecret: pulumi.Output<string>;
  slackSigningSecret: pulumi.Output<string>;
}

function getNatsUrls(
  machine: MachineConfig,
  allMachines: MachineConfig[],
): string {
  const peerInfo = allMachines.map((m) => ({
    name: m.name,
    nats: !!m.nats,
  }));
  return computeNatsUrls(machine.name, !!machine.nats, peerInfo);
}

/**
 * Create the listener container on a machine.
 * Runs on ALL machines (host networking).
 */
export function createListener(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  images: MachineImages,
  dependsOn: pulumi.Resource[],
): docker.Container {
  const natsUrls = getNatsUrls(machine, allMachines);

  return new docker.Container(
    `listener-${machine.name}`,
    {
      name: "envoy-listener",
      image: images.listener.imageId,
      restart: "unless-stopped",
      networkMode: "host",
      envs: [
        "PORT=9020",
        `ENVOY_MACHINE_ID=${machine.machineId}`,
        `NATS_URLS=${natsUrls}`,
        `ENVOY_REGISTRY_DIR=${machine.listener.registryDir}`,
        "ENVOY_HOST_BRIDGE=127.0.0.1",
      ],
      volumes: [
        {
          hostPath: machine.listener.registryDir,
          containerPath: machine.listener.registryDir,
          readOnly: true,
        },
      ],
      healthcheck: {
        tests: ["CMD", "curl", "-f", "http://127.0.0.1:9020/healthz"],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "5s",
      },
      wait: true,
      waitTimeout: 30,
    },
    { provider, dependsOn, deleteBeforeReplace: true },
  );
}

/**
 * Create the GitHub webhook receiver container.
 * Only on machines with receivers.github = true.
 */
export function createGithubReceiver(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  images: MachineImages,
  secrets: ServiceSecrets,
  dependsOn: pulumi.Resource[],
): docker.Container {
  const natsUrls = getNatsUrls(machine, allMachines);

  return new docker.Container(
    `github-${machine.name}`,
    {
      name: "envoy-github",
      image: images.github!.imageId,
      restart: "unless-stopped",
      networkMode: "host",
      envs: [
        "PORT=9010",
        `ENVOY_MACHINE_ID=${machine.machineId}`,
        `NATS_URLS=${natsUrls}`,
        pulumi.interpolate`ENVOY_GITHUB_WEBHOOK_SECRET=${secrets.githubWebhookSecret}`,
        "ENVOY_GITHUB_MENTION_TRIGGER=@legion",
      ],
      healthcheck: {
        tests: ["CMD", "curl", "-f", "http://127.0.0.1:9010/healthz"],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "5s",
      },
      wait: true,
      waitTimeout: 30,
    },
    { provider, dependsOn, deleteBeforeReplace: true },
  );
}

/**
 * Create the Slack webhook receiver container.
 * Only on machines with receivers.slack = true.
 */
export function createSlackReceiver(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  images: MachineImages,
  secrets: ServiceSecrets,
  dependsOn: pulumi.Resource[],
): docker.Container {
  const natsUrls = getNatsUrls(machine, allMachines);

  return new docker.Container(
    `slack-${machine.name}`,
    {
      name: "envoy-slack",
      image: images.slack!.imageId,
      restart: "unless-stopped",
      networkMode: "host",
      envs: [
        "PORT=9011",
        `ENVOY_MACHINE_ID=${machine.machineId}`,
        `NATS_URLS=${natsUrls}`,
        pulumi.interpolate`ENVOY_SLACK_SIGNING_SECRET=${secrets.slackSigningSecret}`,
      ],
      healthcheck: {
        tests: ["CMD", "curl", "-f", "http://127.0.0.1:9011/healthz"],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "5s",
      },
      wait: true,
      waitTimeout: 30,
    },
    { provider, dependsOn, deleteBeforeReplace: true },
  );
}
```

> **Critical:** All containers use `deleteBeforeReplace: true` (host networking = port conflicts on create-before-delete). Secrets use `pulumi.interpolate` to keep them marked as secret in Pulumi state/logs.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd packages/envoy/infra && bun test __tests__/services.test.ts`
Expected: All tests PASS.

- [ ] **Step 5: Verify types compile**

Run: `cd packages/envoy/infra && bunx tsc --noEmit`
Expected: Clean exit, no errors.

- [ ] **Step 6: Commit**

```bash
jj describe -m "feat(envoy): add services module with listener, GitHub, and Slack containers"
jj new
```

---

### Task 6: Entry point, stack config, and secrets — Depends on: Task 3, Task 5

**Files:**
- Create: `packages/envoy/infra/index.ts`
- Create: `packages/envoy/infra/Pulumi.prod.yaml`

- [ ] **Step 1: Create index.ts**

```typescript
import * as pulumi from "@pulumi/pulumi";
import type { MachineConfig } from "./machines";
import { createProvider } from "./machines";
import { pullImages } from "./images";
import { createNatsPeer } from "./nats";
import {
  createGithubReceiver,
  createListener,
  createSlackReceiver,
} from "./services";

const cfg = new pulumi.Config("envoy");

// Stack configuration
const registry = cfg.require("registry");
const imageTag = cfg.require("imageTag");
const natsImage = cfg.get("natsImage") ?? "nats:2.11-alpine";
const machines = cfg.requireObject<MachineConfig[]>("machines");

// Secrets
const githubWebhookSecret = cfg.requireSecret("githubWebhookSecret");
const slackSigningSecret = cfg.requireSecret("slackSigningSecret");

const secrets = { githubWebhookSecret, slackSigningSecret };

// Deploy to each machine
for (const machine of machines) {
  const provider = createProvider(machine);
  const images = pullImages(provider, machine, registry, imageTag, natsImage);

  // NATS peer — only on machines with nats config
  const natsDependency: pulumi.Resource[] = [];
  if (machine.nats && images.nats) {
    const nats = createNatsPeer(provider, machine, machines, images.nats);
    natsDependency.push(nats);
  }

  // Listener — on ALL machines
  createListener(provider, machine, machines, images, natsDependency);

  // Receivers — only where configured
  if (machine.receivers?.github && images.github) {
    createGithubReceiver(
      provider,
      machine,
      machines,
      images,
      secrets,
      natsDependency,
    );
  }

  if (machine.receivers?.slack && images.slack) {
    createSlackReceiver(
      provider,
      machine,
      machines,
      images,
      secrets,
      natsDependency,
    );
  }
}
```

- [ ] **Step 2: Initialize the Pulumi stack**

Run: `cd packages/envoy/infra && pulumi stack init prod`
Expected: Stack `prod` created.

- [ ] **Step 3: Create Pulumi.prod.yaml with machine configs**

```yaml
config:
  envoy:registry: ghcr.io/sjawhar/legion
  envoy:imageTag: PLACEHOLDER_SET_BEFORE_DEPLOY
  envoy:natsImage: "nats:2.11-alpine"
  envoy:machines:
    - name: sami-agents-mx
      sshHost: "ssh://ubuntu@sami-agents-mx"
      machineId: sami-agents-mx
      nats:
        serverName: sami-agents-mx
      listener:
        registryDir: /home/ubuntu/.local/state/opencode/registry
      receivers:
        github: true
        slack: true
    - name: sami
      sshHost: "ssh://sami@sami"
      machineId: sami
      nats:
        serverName: sami
      listener:
        registryDir: /home/sami/.local/state/opencode/registry
    - name: sami-claude
      sshHost: "ssh://claude@sami-claude"
      machineId: sami-claude
      nats:
        serverName: sami-claude
      listener:
        registryDir: /home/claude/.local/state/opencode/registry
    - name: ghost-wispr
      sshHost: "ssh://ghost-wispr@ghost-wispr"
      machineId: ghost-wispr
      listener:
        registryDir: /home/ghost-wispr/.local/state/opencode/registry
```

> **Note:** `envoy:imageTag` is a placeholder. Set to actual git SHA before deploy: `pulumi config set envoy:imageTag <sha>`.

- [ ] **Step 4: Set secrets via CLI**

```bash
cd packages/envoy/infra
pulumi config set --secret envoy:githubWebhookSecret "$(../scripts/with-envoy-secrets printenv ENVOY_GITHUB_WEBHOOK_SECRET)"
pulumi config set --secret envoy:slackSigningSecret "$(../scripts/with-envoy-secrets printenv ENVOY_SLACK_SIGNING_SECRET)"
```

Expected: `Pulumi.prod.yaml` now contains `secure:` encrypted values for both secrets.

- [ ] **Step 5: Run full type-check**

Run: `cd packages/envoy/infra && bunx tsc --noEmit`
Expected: Clean exit, no errors.

- [ ] **Step 6: Run all unit tests**

Run: `cd packages/envoy/infra && bun test`
Expected: All tests pass (NATS config rendering + NATS URL computation).

- [ ] **Step 7: Commit**

```bash
jj describe -m "feat(envoy): add Pulumi entry point, stack config, and encrypted secrets"
jj new
```

---

### Task 7: Multi-arch image build script — Independent

**Files:**
- Create: `packages/envoy/scripts/build-push-images.sh`

- [ ] **Step 1: Create build-push-images.sh**

```bash
#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."

TAG="${1:?Usage: build-push-images.sh <git-sha-tag>}"
REGISTRY="${ENVOY_REGISTRY:-ghcr.io/sjawhar/legion}"

echo "Building and pushing multi-arch images with tag: $TAG"
echo "Registry: $REGISTRY"

for svc in listener github slack; do
  echo ""
  echo "=== Building envoy-${svc}:${TAG} ==="
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    -t "${REGISTRY}/envoy-${svc}:${TAG}" \
    -f "docker/${svc}.Dockerfile" \
    --push \
    .
  echo "=== Pushed ${REGISTRY}/envoy-${svc}:${TAG} ==="
done

echo ""
echo "All images built and pushed with tag: $TAG"
echo "Next steps:"
echo "  cd infra"
echo "  pulumi config set envoy:imageTag $TAG"
echo "  pulumi preview"
echo "  pulumi up"
```

- [ ] **Step 2: Make executable**

Run: `chmod +x packages/envoy/scripts/build-push-images.sh`

- [ ] **Step 3: Commit**

```bash
jj describe -m "feat(envoy): add multi-arch image build and push script"
jj new
```

---

### Task 8: Validation, migration procedure, and smoke tests — Depends on: Task 6

**Files:** None (validation + documentation in this task)

- [ ] **Step 1: Run Pulumi preview**

Run: `cd packages/envoy/infra && pulumi preview --stack prod`
Expected: Preview shows resources to create including:
- `docker:index/provider:Provider` resources (one per machine)
- `docker:index/remoteImage:RemoteImage` resources (only needed images per machine)
- `docker:index/volume:Volume` on NATS peer machines
- `docker:index/container:Container` for each service

Preview should complete without TypeScript errors. It may fail to connect to remote Docker daemons — that is expected at development time and will be resolved during the actual rolling migration.

- [ ] **Step 2: Preflight — verify each machine**

Before any migration, run this preflight on each host:

```bash
for host in ubuntu@sami-agents-mx sami@sami claude@sami-claude ghost-wispr@ghost-wispr; do
  echo "=== $host ==="
  echo -n "  Docker: "
  ssh -o ConnectTimeout=5 "$host" docker version --format '{{.Server.Version}}' 2>&1 || echo "UNREACHABLE"
  echo -n "  Arch: "
  ssh "$host" uname -m 2>&1 || echo "UNKNOWN"
  echo -n "  NATS volume: "
  ssh "$host" docker volume ls --filter name=nats --format '{{.Name}}' 2>&1 || echo "NONE"
  echo -n "  Running compose containers: "
  ssh "$host" docker ps --filter name=envoy --format '{{.Names}}' 2>&1 | tr '\n' ' ' || echo "NONE"
  echo ""
done
```

Expected: All 4 hosts reachable, Docker running, NATS volume name confirmed (likely `nats_nats_data` on peer machines). If the actual volume name differs from `nats_nats_data`, update the `name` field in `nats.ts` before proceeding.

- [ ] **Step 3: Rolling migration — per-host cutover**

Migrate one NATS peer at a time, maintaining 2/3 quorum:

**Machine 1 (sami-agents-mx):**
```bash
# Stop old compose services FIRST to free ports
ssh ubuntu@sami-agents-mx 'cd ~/legion/default/packages/envoy/deploy && \
  docker compose -f compose/nats/peer.compose.yml down && \
  docker compose -f compose/listener.compose.yml down && \
  docker compose -f compose/github.compose.yml down && \
  docker compose -f compose/slack.compose.yml down'

# Deploy via Pulumi (use resource names from pulumi preview output)
cd packages/envoy/infra
pulumi up --stack prod \
  --target 'urn:pulumi:prod::envoy::docker:index/provider:Provider::docker-sami-agents-mx' \
  --target-dependents \
  --yes

# Verify
curl -sf http://sami-agents-mx:8222/healthz && echo "NATS OK" || echo "NATS FAIL"
curl -sf http://sami-agents-mx:9020/healthz && echo "Listener OK" || echo "Listener FAIL"
curl -sf http://sami-agents-mx:9010/healthz && echo "GitHub OK" || echo "GitHub FAIL"
curl -sf http://sami-agents-mx:9011/healthz && echo "Slack OK" || echo "Slack FAIL"
```

**Machine 2 (sami):**
```bash
ssh sami@sami 'cd ~/legion/default/packages/envoy/deploy && \
  docker compose -f compose/nats/peer.compose.yml down && \
  docker compose -f compose/listener.compose.yml down'

pulumi up --stack prod \
  --target 'urn:pulumi:prod::envoy::docker:index/provider:Provider::docker-sami' \
  --target-dependents \
  --yes

curl -sf http://sami:8222/healthz && echo "NATS OK" || echo "NATS FAIL"
curl -sf http://sami:9020/healthz && echo "Listener OK" || echo "Listener FAIL"
```

**Verify 2-peer NATS cluster after machines 1+2:**
```bash
for host in sami-agents-mx sami; do
  echo -n "$host routes: "
  curl -sf "http://$host:8222/routez" | jq '.routes | length'
done
```
Expected: Each returns `1` (connected to the other peer).

**Machine 3 (sami-claude):**
```bash
ssh claude@sami-claude 'cd ~/legion/default/packages/envoy/deploy && \
  docker compose -f compose/nats/peer.compose.yml down && \
  docker compose -f compose/listener.compose.yml down'

pulumi up --stack prod \
  --target 'urn:pulumi:prod::envoy::docker:index/provider:Provider::docker-sami-claude' \
  --target-dependents \
  --yes

curl -sf http://sami-claude:8222/healthz && echo "NATS OK" || echo "NATS FAIL"
curl -sf http://sami-claude:9020/healthz && echo "Listener OK" || echo "Listener FAIL"
```

**Machine 4 (ghost-wispr — listener only, no NATS):**
```bash
ssh ghost-wispr@ghost-wispr 'cd ~/legion/default/packages/envoy/deploy && \
  docker compose -f compose/listener.compose.yml down'

pulumi up --stack prod \
  --target 'urn:pulumi:prod::envoy::docker:index/provider:Provider::docker-ghost-wispr' \
  --target-dependents \
  --yes

curl -sf http://ghost-wispr:9020/healthz && echo "Listener OK" || echo "Listener FAIL"
```

> **Note on `--target` URNs:** The exact URN for each provider is deterministic from the resource name in the code: `urn:pulumi:prod::envoy::docker:index/provider:Provider::docker-{machine-name}`. Using `--target-dependents` deploys all resources that depend on that provider (images, volumes, containers for that machine).

- [ ] **Step 4: Post-migration verification**

```bash
# Full NATS cluster — all 3 peers show 2 routes each
for host in sami-agents-mx sami sami-claude; do
  echo -n "$host routes: "
  curl -sf "http://$host:8222/routez" | jq '.routes | length'
done
# Expected: each returns 2

# All health checks
for host in sami-agents-mx sami sami-claude ghost-wispr; do
  echo -n "$host listener: "
  curl -sf "http://$host:9020/healthz" && echo "OK" || echo "FAIL"
done
echo -n "sami-agents-mx github: "
curl -sf "http://sami-agents-mx:9010/healthz" && echo "OK" || echo "FAIL"
echo -n "sami-agents-mx slack: "
curl -sf "http://sami-agents-mx:9011/healthz" && echo "OK" || echo "FAIL"
# Expected: all OK

# Image digest consistency
for host in ubuntu@sami-agents-mx sami@sami claude@sami-claude ghost-wispr@ghost-wispr; do
  echo -n "$host envoy-listener: "
  ssh "$host" docker inspect envoy-listener --format '{{.Image}}' 2>&1
done
# Expected: all 4 return identical SHA digest

# JetStream data preservation
curl -sf http://sami-agents-mx:8222/jsz?streams=true | jq '.streams[] | {name: .name, state: .state}'
# Expected: ENVOY_NOTIFICATIONS stream with consumer data intact

# NATS cluster replication verification (cross-peer connectivity)
# Use wget from inside a NATS container to query another peer's monitoring
ssh ubuntu@sami-agents-mx 'docker exec envoy-nats wget -qO- http://sami:8222/routez' | jq '.routes | length'
# Expected: 2 (proves cross-peer NATS monitoring is reachable)

# Idempotency check
pulumi preview --stack prod
# Expected: no changes detected
```

- [ ] **Step 5: Rollback procedure (document, do not execute)**

If any machine fails during migration:
```bash
# 1. Destroy Pulumi resources for the failed machine
pulumi destroy --stack prod \
  --target 'urn:pulumi:prod::envoy::docker:index/provider:Provider::docker-{machine-name}' \
  --target-dependents \
  --yes

# 2. Restart old compose services
ssh user@machine 'cd ~/legion/default/packages/envoy/deploy && \
  ./scripts/up-nats-peer.sh && \
  ./scripts/up-listener.sh'
```

- [ ] **Step 6: Commit final plan and any adjustments**

```bash
jj describe -m "feat(envoy): complete Pulumi IaC with migration procedure and smoke tests"
jj new
```

---

## Testing Plan

### Setup

```bash
cd packages/envoy/infra
bun install
pulumi stack select prod

TAG=$(jj log -r @ --no-graph -T 'commit_id.short(8)')
cd .. && ./scripts/build-push-images.sh "$TAG"
cd infra && pulumi config set envoy:imageTag "$TAG"
```

### Health Check

```bash
for host in ubuntu@sami-agents-mx sami@sami claude@sami-claude ghost-wispr@ghost-wispr; do
  echo -n "$host: "
  ssh -o ConnectTimeout=5 "$host" docker version --format '{{.Server.Version}}' 2>&1 || echo "UNREACHABLE"
done
```

Expected: All 4 return Docker version. Timeout: retry up to 30s per host.

### Verification Steps

**1. NATS cluster health** — All 3 peers show 2 routes each:
- Action: `for host in sami-agents-mx sami sami-claude; do curl -sf "http://$host:8222/routez" | jq '.routes | length'; done`
- Expected: `2` for each peer
- Tool: curl + jq

**2. All service health checks pass:**
- Action: `for host in sami-agents-mx sami sami-claude ghost-wispr; do curl -sf "http://$host:9020/healthz"; done && curl -sf http://sami-agents-mx:9010/healthz && curl -sf http://sami-agents-mx:9011/healthz`
- Expected: All return HTTP 200
- Tool: curl

**3. Image digest consistency across machines:**
- Action: `for host in ubuntu@sami-agents-mx sami@sami claude@sami-claude ghost-wispr@ghost-wispr; do ssh "$host" docker inspect envoy-listener --format '{{.Image}}'; done`
- Expected: All 4 return identical SHA digest
- Tool: ssh + docker inspect

**4. JetStream data preserved:**
- Action: `curl -sf http://sami-agents-mx:8222/jsz?streams=true | jq '.streams[] | select(.name=="ENVOY_NOTIFICATIONS") | {name: .name, messages: .state.messages, consumers: .state.consumer_count}'`
- Expected: Stream exists with messages and consumers intact
- Tool: curl + jq

**5. Cross-peer NATS replication (e2e remote test):**
- Action: `ssh ubuntu@sami-agents-mx 'docker exec envoy-nats wget -qO- http://sami:8222/routez' | jq '.routes | length'`
- Expected: `2` (proves NATS container on sami-agents-mx can reach sami's NATS monitoring)
- Tool: ssh + wget (via NATS container) + jq

**6. Idempotency:**
- Action: `pulumi preview --stack prod`
- Expected: No changes detected
- Tool: Pulumi CLI

### Tools Needed

- **Pulumi CLI** — stack management, deploy, preview
- **curl** — health checks, NATS monitoring API
- **jq** — JSON parsing for route/stream verification
- **ssh** — remote Docker inspect, in-container commands
- **docker buildx** — multi-arch image builds

---

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | `using-jj` |
| Test | `verification-before-completion` |
| Review | (none beyond standard) |

Workers: invoke these skills at the start of your workflow before beginning work.
