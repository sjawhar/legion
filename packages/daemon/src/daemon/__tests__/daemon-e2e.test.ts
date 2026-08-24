import { describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import os from "node:os";
import path from "node:path";
import {
  controllerToken,
  formatIssueKey,
  roleToken,
  roleTopic,
  sanitizeToken,
} from "@legion/contracts";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import type { DaemonConfig } from "../config";
import { type DaemonHandle, startDaemon } from "../index";

const NATS_IMAGE = "nats:2.10";
const EXTENSION_PACKAGE = path.resolve(import.meta.dir, "../../../../envoy-omp-extension");

interface CommandResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

interface NatsEvent {
  subject: string;
  payload: unknown;
}

interface NatsContainer {
  name: string;
  url: string;
}

async function run(command: string[]): Promise<CommandResult> {
  const process = Bun.spawn(command, { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
    process.exited,
  ]);
  return { stdout, stderr, exitCode };
}

async function scratchPort(): Promise<number> {
  const server = createServer();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = server.address();
  if (!address || typeof address === "string") {
    server.close();
    throw new Error("Unable to reserve a TCP port");
  }
  await new Promise<void>((resolve, reject) => {
    server.close((error) => (error ? reject(error) : resolve()));
  });
  return address.port;
}

function envelope(
  eventId: string,
  source: "envoy" | "github",
  topic: string,
  payload: Record<string, unknown>
) {
  return {
    event_id: eventId,
    source,
    source_event_id: eventId,
    topic,
    dedupe_key: `${source}.${eventId}`,
    issued_at: Date.now(),
    payload_summary: "daemon e2e test event",
    payload: JSON.stringify(payload),
    trace_id: `trace-${eventId}`,
  };
}

async function startNats(port: number): Promise<NatsContainer> {
  const name = `legion-daemon-e2e-${process.pid}-${Date.now()}`;
  const result = await run([
    "docker",
    "run",
    "-d",
    "--name",
    name,
    "-p",
    `127.0.0.1:${port}:4222`,
    NATS_IMAGE,
  ]);
  if (result.exitCode !== 0) {
    throw new Error(`Unable to start NATS container ${name}: ${result.stderr || result.stdout}`);
  }
  return { name, url: `nats://127.0.0.1:${port}` };
}

async function stopNats(name: string): Promise<void> {
  const stopped = await run(["docker", "stop", "--timeout", "1", name]);
  const removed = await run(["docker", "rm", name]);
  if (removed.exitCode !== 0) {
    throw new Error(`Unable to remove NATS container ${name}: ${removed.stderr || removed.stdout}`);
  }
  if (stopped.exitCode !== 0) {
    throw new Error(`Unable to stop NATS container ${name}: ${stopped.stderr || stopped.stdout}`);
  }
}

async function exists(target: string): Promise<boolean> {
  try {
    await stat(target);
    return true;
  } catch {
    return false;
  }
}

async function createPromptFixtures(): Promise<() => Promise<void>> {
  const promptDir = path.join(EXTENSION_PACKAGE, "agents");
  const createdDir = !(await exists(promptDir));
  const createdFiles: string[] = [];
  await mkdir(promptDir, { recursive: true });

  for (const prompt of ["architect-root.md", "controller-root.md"]) {
    const fixture = path.join(promptDir, prompt);
    if (await exists(fixture)) continue;
    await writeFile(fixture, `# ${prompt} fixture\n`, "utf8");
    createdFiles.push(fixture);
  }

  return async () => {
    await Promise.all(createdFiles.map((fixture) => rm(fixture, { force: true })));
    if (createdDir) await rm(promptDir, { recursive: true, force: true });
  };
}

function config(stateDir: string, port: number, natsUrl: string, project: string): DaemonConfig {
  return {
    project,
    legionId: "acme/1",
    port,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: [natsUrl],
    dispatchUrl: "http://127.0.0.1:13380",
    dispatchBearer: "dispatch-bearer",
    boardProjectIds: ["PVT_board"],
    appLogins: ["legion-implement[bot]", "legion-review[bot]"],
    admissionCap: 1,
    workerBudget: 2,
    maxRecursionDepth: 8,
    lingerHours: 72,
    ciQuietMs: 5_000,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir,
  };
}

async function post(url: string, body: Record<string, unknown>): Promise<Response> {
  const response = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  expect(response.status).toBe(200);
  return response;
}

describe("daemon end-to-end", () => {
  it.skipIf(process.env.LEGION_E2E !== "1")(
    "routes a GitHub event through admission and revives an unclaimed worker over live NATS",
    async () => {
      const project = `e2e${Date.now()}`;
      const root = formatIssueKey("acme", "widgets", 1);
      const child = formatIssueKey("acme", "widgets", 2);
      const controller = controllerToken(project);
      const childArchitect = roleToken(project, child, "architect");
      const worker = roleToken(project, child, "implementer");
      const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-e2e-"));
      const natsPort = await scratchPort();
      const daemonPort = await scratchPort();
      const removePromptFixtures = await createPromptFixtures();
      let nats: NatsContainer | undefined;
      let broker: NatsConnection | undefined;
      let daemon: DaemonHandle | undefined;
      const windows = new Set<string>();
      let tmuxSessionExists = false;
      const controllerSpawn = Promise.withResolvers<string[]>();
      const rootSpawn = Promise.withResolvers<string[]>();
      const controllerTriage = Promise.withResolvers<NatsEvent>();
      const childArchitectPublication = Promise.withResolvers<NatsEvent>();
      const workerPublication = Promise.withResolvers<NatsEvent>();
      const workerDirective = Promise.withResolvers<NatsEvent>();
      let roleSubscription: Subscription | undefined;
      let controlSubscription: Subscription | undefined;
      let rolePump: Promise<void> | undefined;
      let controlPump: Promise<void> | undefined;

      const runner = async (command: string[]): Promise<CommandResult> => {
        if (command[0] === "kill") return { stdout: "", stderr: "", exitCode: 0 };
        if (command[0] !== "tmux") {
          throw new Error(`Fake GitHub runner received unexpected command: ${command.join(" ")}`);
        }
        if (command[1] === "has-session") {
          return { stdout: "", stderr: "", exitCode: tmuxSessionExists ? 0 : 1 };
        }
        if (command[1] === "new-session") {
          tmuxSessionExists = true;
          return { stdout: "", stderr: "", exitCode: 0 };
        }
        if (command[1] === "new-window") {
          const nameIndex = command.indexOf("-n");
          const window = command[nameIndex + 1];
          if (!window)
            throw new Error(`tmux new-window command is missing -n: ${command.join(" ")}`);
          windows.add(window);
          if (window === "controller") controllerSpawn.resolve([...command]);
          if (window === "acme-widgets-1") rootSpawn.resolve([...command]);
          return { stdout: "", stderr: "", exitCode: 0 };
        }
        if (command[1] === "list-windows") {
          return windows.size > 0
            ? { stdout: `${[...windows].join("\n")}\n`, stderr: "", exitCode: 0 }
            : { stdout: "", stderr: "", exitCode: 1 };
        }
        if (command[1] === "list-panes") {
          const target = command[3]?.split(":").at(-1);
          return target && windows.has(target)
            ? { stdout: "4242\n", stderr: "", exitCode: 0 }
            : { stdout: "", stderr: "", exitCode: 1 };
        }
        throw new Error(`Unhandled tmux command: ${command.join(" ")}`);
      };

      try {
        nats = await startNats(natsPort);
        // Docker's NATS startup is an external process event; wait for its protocol handshake, not a fixed sleep.
        broker = await connect({
          servers: [nats.url],
          name: `legion-daemon-e2e-observer-${project}`,
          waitOnFirstConnect: true,
          maxReconnectAttempts: -1,
          reconnectTimeWait: 20,
        });
        const codec = StringCodec();
        const role = broker.subscribe("notifications.role.>");
        const control = broker.subscribe("legion.ctl.>");
        roleSubscription = role;
        controlSubscription = control;
        rolePump = (async () => {
          for await (const message of role) {
            const event = {
              subject: message.subject,
              payload: JSON.parse(codec.decode(message.data)),
            };
            if (event.subject === roleTopic(controller)) controllerTriage.resolve(event);
            if (event.subject === roleTopic(childArchitect))
              childArchitectPublication.resolve(event);
            if (event.subject === roleTopic(worker)) workerPublication.resolve(event);
          }
        })();
        controlPump = (async () => {
          for await (const message of control) {
            const event = {
              subject: message.subject,
              payload: JSON.parse(codec.decode(message.data)),
            };
            if (
              event.subject.startsWith("legion.ctl.") &&
              typeof event.payload === "object" &&
              event.payload !== null &&
              "type" in event.payload &&
              event.payload.type === "revive-worker"
            ) {
              workerDirective.resolve(event);
            }
          }
        })();
        await broker.flush();

        daemon = await startDaemon(config(stateDir, daemonPort, nats.url, project), {
          deps: {
            runner,
            envoyPublish: async (topic, payload) => {
              broker?.publish(topic, codec.encode(payload));
              await broker?.flush();
            },
            fetchGitHubProjectItems: async () => ({ items: [] }),
            onSignal: () => {},
          },
        });
        await daemon.ready();
        const rootTopic = "notifications.github.acme.widgets.issue.1";
        broker.publish(
          rootTopic,
          codec.encode(
            JSON.stringify(
              envelope("issue-opened", "github", rootTopic, {
                action: "opened",
                repository: { full_name: "acme/widgets" },
                project: { id: "PVT_board" },
                issue: {
                  number: 1,
                  title: "Root issue",
                  state: "open",
                  labels: [],
                  sub_issues: [{ number: 2, title: "Child issue", state: "open", labels: [] }],
                },
              })
            )
          )
        );
        await broker.flush();

        const triage = await controllerTriage.promise;
        expect(triage.payload).toMatchObject({ type: "triage", issue: root });

        const controllerExceptionTopic = `notifications.envoy.exceptions.notifications.role.${controller}`;
        broker.publish(
          controllerExceptionTopic,
          codec.encode(
            JSON.stringify(
              envelope("controller-unclaimed", "envoy", controllerExceptionTopic, {
                original_topic: roleTopic(controller),
                event_id: "issue-opened",
                payload: JSON.stringify(triage.payload),
                reason: "no_holder",
              })
            )
          )
        );
        await broker.flush();

        const controllerSpawnArgv = await controllerSpawn.promise;
        const controllerSecret = controllerSpawnArgv
          .find((argument) => argument.startsWith("LEGION_CONTROLLER_SECRET="))
          ?.slice("LEGION_CONTROLLER_SECRET=".length);
        if (!controllerSecret)
          throw new Error("Controller tmux spawn did not include its capability");

        const daemonUrl = `http://127.0.0.1:${daemon.server.port}`;
        await post(`${daemonUrl}/legion/v1/admission`, { secret: controllerSecret, issue: root });

        const rootSpawnArgv = await rootSpawn.promise;
        expect(rootSpawnArgv).toContain(`LEGION_DAEMON_URL=http://127.0.0.1:${daemonPort}`);
        expect(rootSpawnArgv).toContain(`ENVOY_NATS_URL=${nats.url}`);

        const rootSessionFile = path.join(
          stateDir,
          "trees",
          "acme-widgets-1",
          ".omp",
          "session.json"
        );
        await mkdir(path.dirname(rootSessionFile), { recursive: true });
        await writeFile(rootSessionFile, "{}\n", "utf8");
        const started = await post(`${daemonUrl}/legion/v1/process/started`, {
          tree: root,
          generation: 1,
          rootSessionId: "root-session",
          ompSessionFile: rootSessionFile,
        });
        expect(await started.json()).toMatchObject({
          controlSubject: `legion.ctl.${sanitizeToken(root)}.1`,
        });

        await post(`${daemonUrl}/legion/v1/role-backing`, {
          tree: root,
          issue: child,
          role: "architect",
          agentId: "child-architect",
        });
        await post(`${daemonUrl}/legion/v1/role-backing`, {
          tree: root,
          issue: child,
          role: "implementer",
          agentId: "child-worker",
        });
        await post(`${daemonUrl}/legion/v1/waves/release`, { tree: root, children: [child] });

        const childCommentTopic = "notifications.github.acme.widgets.issue.2.comment";
        broker.publish(
          childCommentTopic,
          codec.encode(
            JSON.stringify(
              envelope("child-comment", "github", childCommentTopic, {
                action: "created",
                repository: { full_name: "acme/widgets" },
                issue: { number: 2 },
                comment: {
                  user: { login: "human" },
                  body: "Please investigate this child",
                  html_url: "https://github.com/acme/widgets/issues/2#issuecomment-1",
                },
              })
            )
          )
        );
        await broker.flush();

        const architectPublication = await childArchitectPublication.promise;
        expect(architectPublication.payload).toMatchObject({
          type: "issue-comment",
          author: "human",
          body: "Please investigate this child",
        });

        const workerPayload = JSON.stringify({ type: "work", issue: child });
        broker.publish(roleTopic(worker), codec.encode(workerPayload));
        await broker.flush();
        const observedWorkerPublication = await workerPublication.promise;
        expect(observedWorkerPublication.payload).toMatchObject({ type: "work", issue: child });

        const workerExceptionTopic = `notifications.envoy.exceptions.notifications.role.${worker}`;
        broker.publish(
          workerExceptionTopic,
          codec.encode(
            JSON.stringify(
              envelope("worker-unclaimed", "envoy", workerExceptionTopic, {
                original_topic: roleTopic(worker),
                event_id: "worker-work",
                payload: workerPayload,
                reason: "no_holder",
              })
            )
          )
        );
        await broker.flush();

        const directive = await workerDirective.promise;
        expect(directive.subject).toBe(`legion.ctl.${sanitizeToken(root)}.1`);
        expect(directive.payload).toEqual({
          type: "revive-worker",
          role: "implementer",
          agentId: "child-worker",
          parentSessionFile: rootSessionFile,
          redeliver: { topic: roleTopic(worker), payload: workerPayload, eventId: "worker-work" },
        });
        await daemon.drain();
      } finally {
        await daemon?.stop();
        roleSubscription?.unsubscribe();
        controlSubscription?.unsubscribe();
        if (rolePump) await rolePump;
        if (controlPump) await controlPump;
        await broker?.drain();
        if (nats) await stopNats(nats.name);
        await removePromptFixtures();
        await rm(stateDir, { recursive: true, force: true });
      }
    }
  );
});
