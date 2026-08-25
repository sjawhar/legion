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
import {
	connect,
	type NatsConnection,
	StringCodec,
	type Subscription,
} from "nats";
import type { DaemonConfig } from "../config";
import type { DaemonEnvironment } from "../environment";
import { type DaemonHandle, startDaemon } from "../index";

const NATS_IMAGE = "nats:2.10";
const NATS_READY_TIMEOUT_MS = 30_000;
const NATS_READY_POLL_MS = 100;
const NATS_CONNECT_TIMEOUT_MS = 1_000;
const E2E_TEST_TIMEOUT_MS = NATS_READY_TIMEOUT_MS + 5_000;

const EXTENSION_PACKAGE = path.resolve(
	import.meta.dir,
	"../../../../envoy-omp-extension",
);

const DAEMON_ENVIRONMENT: DaemonEnvironment = {
	commands: {
		jj: "/tools/jj",
		git: "/tools/git",
		gh: "/tools/gh",
		tmux: "tmux",
	},
	ompInvocation: "/tools/omp",
	paneEnv: { PATH: process.env.PATH ?? "" },
};

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
	payload: Record<string, unknown>,
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
		throw new Error(
			`Unable to start NATS container ${name}: ${result.stderr || result.stdout}`,
		);
	}
	return { name, url: `nats://127.0.0.1:${port}` };
}
async function connectNatsWhenReady(
	url: string,
	name: string,
): Promise<NatsConnection> {
	const deadline = Date.now() + NATS_READY_TIMEOUT_MS;
	let lastError: unknown;

	while (Date.now() < deadline) {
		try {
			return await connect({
				servers: [url],
				name,
				reconnect: false,
				timeout: NATS_CONNECT_TIMEOUT_MS,
			});
		} catch (error) {
			lastError = error;
			// Docker readiness follows the platform clock; delay to avoid busy-polling real connections.
			await Bun.sleep(NATS_READY_POLL_MS);
		}
	}

	const reason =
		lastError instanceof Error ? lastError.message : String(lastError);
	throw new Error(
		`NATS at ${url} did not accept a connection within ${NATS_READY_TIMEOUT_MS}ms: ${reason}`,
	);
}

async function stopNats(name: string): Promise<void> {
	const stopped = await run(["docker", "stop", "--timeout", "1", name]);
	const removed = await run(["docker", "rm", name]);
	if (removed.exitCode !== 0) {
		throw new Error(
			`Unable to remove NATS container ${name}: ${removed.stderr || removed.stdout}`,
		);
	}
	if (stopped.exitCode !== 0) {
		throw new Error(
			`Unable to stop NATS container ${name}: ${stopped.stderr || stopped.stdout}`,
		);
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
		await Promise.all(
			createdFiles.map((fixture) => rm(fixture, { force: true })),
		);
		if (createdDir) await rm(promptDir, { recursive: true, force: true });
	};
}

function config(
	stateDir: string,
	port: number,
	natsUrl: string,
	project: string,
): DaemonConfig {
	return {
		project,
		legionId: "acme/1",
		port,
		envoyUrl: "http://127.0.0.1:9020",
		natsUrls: [natsUrl],
		dispatchUrl: "http://127.0.0.1:13380",
		dispatchBearer: "dispatch-bearer",
		ompInvocation:
			"mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
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

async function post(
	url: string,
	body: Record<string, unknown>,
): Promise<Response> {
	const response = await fetch(url, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: JSON.stringify(body),
	});
	expect(response.status).toBe(200);
	return response;
}

describe("daemon end-to-end", () => {
	// Requires Docker to create an isolated NATS broker.
	it.skipIf(process.env.LEGION_E2E !== "1")(
		"routes a GitHub event through admission and revives an unclaimed worker over live NATS",
		async () => {
			const project = `e2e${Date.now()}`;
			const root = formatIssueKey("acme", "widgets", 1);
			const child = formatIssueKey("acme", "widgets", 2);
			const controller = controllerToken(project);
			const childArchitect = roleToken(project, child, "architect");
			const worker = roleToken(project, child, "implementer");
			const stateDir = await mkdtemp(
				path.join(os.tmpdir(), "legion-daemon-e2e-"),
			);
			const natsPort = await scratchPort();
			const daemonPort = await scratchPort();
			const removePromptFixtures = await createPromptFixtures();
			let nats: NatsContainer | undefined;
			let broker: NatsConnection | undefined;
			let daemon: DaemonHandle | undefined;
			const windows = new Map<string, string>();
			let tmuxSessionExists = false;
			let nextWindowId = 1;
			const controllerSpawn = Promise.withResolvers<string[]>();
			const rootSpawn = Promise.withResolvers<string[]>();
			const rootRespawn = Promise.withResolvers<string[]>();
			let rootSpawnCount = 0;
			const controllerTriage = Promise.withResolvers<NatsEvent>();
			const childArchitectPublication = Promise.withResolvers<NatsEvent>();
			const workerPublication = Promise.withResolvers<NatsEvent>();
			const workerPublications: NatsEvent[] = [];
			const workerDirective = Promise.withResolvers<NatsEvent>();
			let roleSubscription: Subscription | undefined;
			let controlSubscription: Subscription | undefined;
			let rolePump: Promise<void> | undefined;
			let controlPump: Promise<void> | undefined;

			const runner = async (command: string[]): Promise<CommandResult> => {
				if (command[0] === "sh") {
					return {
						stdout: "",
						stderr: "LEGION_OMP_AGENTS=available\n",
						exitCode: 0,
					};
				}
				if (
					command[0] === "/tools/jj" &&
					command[1] === "git" &&
					command[2] === "clone"
				) {
					const cloneDir = command.at(-1);
					if (!cloneDir)
						throw new Error("Fake Jujutsu clone is missing its destination");
					await mkdir(path.join(cloneDir, ".jj", "repo", "store", "git"), {
						recursive: true,
					});
					return { stdout: "", stderr: "", exitCode: 0 };
				}
				if (
					command[0] === "/tools/jj" &&
					command[1] === "workspace" &&
					command[2] === "add"
				) {
					const workspaceDir = command[3];
					if (!workspaceDir)
						throw new Error(
							"Fake Jujutsu workspace is missing its destination",
						);
					await mkdir(workspaceDir, { recursive: true });
					return { stdout: "", stderr: "", exitCode: 0 };
				}
				if (command[0] === "/tools/jj" || command[0] === "/tools/git") {
					return { stdout: "", stderr: "", exitCode: 0 };
				}
				if (command[0] === "kill")
					return { stdout: "", stderr: "", exitCode: 0 };
				if (command[0] === "gh" || command[0] === "/tools/gh") {
					return { stdout: "[]", stderr: "", exitCode: 0 };
				}
				if (command[0] !== "tmux") {
					throw new Error(
						`Fake GitHub runner received unexpected command: ${command.join(" ")}`,
					);
				}
				if (command[1] === "has-session") {
					return {
						stdout: "",
						stderr: "",
						exitCode: tmuxSessionExists ? 0 : 1,
					};
				}
				if (command[1] === "new-session" || command[1] === "new-window") {
					const nameIndex = command.indexOf("-n");
					const window = command[nameIndex + 1];
					if (!window)
						throw new Error(
							`tmux ${command[1]} command is missing -n: ${command.join(" ")}`,
						);
					const windowId = `@${nextWindowId++}`;
					tmuxSessionExists = true;
					windows.set(windowId, window);
					if (window === "controller") controllerSpawn.resolve([...command]);
					if (window === "acme__widgets-1") {
						rootSpawnCount += 1;
						if (rootSpawnCount === 1) rootSpawn.resolve([...command]);
						else rootRespawn.resolve([...command]);
					}
					return { stdout: `${windowId}\n`, stderr: "", exitCode: 0 };
				}
				if (command[1] === "set-option") {
					return { stdout: "", stderr: "", exitCode: 0 };
				}
				if (command[1] === "list-windows") {
					const format = command[command.indexOf("-F") + 1];
					const values =
						format === "#{window_id}"
							? [...windows.keys()]
							: [...windows.values()];
					return values.length > 0
						? { stdout: `${values.join("\n")}\n`, stderr: "", exitCode: 0 }
						: { stdout: "", stderr: "", exitCode: 1 };
				}
				if (command[1] === "list-panes") {
					const target = command[3];
					return target && windows.has(target)
						? { stdout: "4242\n", stderr: "", exitCode: 0 }
						: { stdout: "", stderr: "", exitCode: 1 };
				}
				throw new Error(`Unhandled tmux command: ${command.join(" ")}`);
			};

			try {
				nats = await startNats(natsPort);
				broker = await connectNatsWhenReady(
					nats.url,
					`legion-daemon-e2e-observer-${project}`,
				);
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
						if (event.subject === roleTopic(controller))
							controllerTriage.resolve(event);
						if (event.subject === roleTopic(childArchitect))
							childArchitectPublication.resolve(event);
						if (event.subject === roleTopic(worker)) {
							workerPublications.push(event);
							workerPublication.resolve(event);
						}
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
							if (message.reply) {
								message.respond(codec.encode(JSON.stringify({ type: "ack" })));
							}
						}
					}
				})();
				await broker.flush();

				daemon = await startDaemon(
					config(stateDir, daemonPort, nats.url, project),
					{
						deps: {
							resolveDaemonEnvironment: async () => DAEMON_ENVIRONMENT,
							runner,
							readProcessCmdline: async () => "omp\0",
							envoyPublish: async (topic, payload) => {
								broker?.publish(topic, codec.encode(payload));
								await broker?.flush();
							},
							fetchGitHubProjectItems: async () => ({
								items: [],
								excludedNullContentItems: 0,
							}),
							tokenManager: {
								getToken: async () => ({
									token: "test-token",
									expiresAt: "2099-01-01T00:00:00.000Z",
									gitIdentity: {
										name: "legion-implement[bot]",
										email: "42+legion-implement[bot]@users.noreply.github.com",
									},
								}),
							},
							onSignal: () => {},
						},
					},
				);
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
									sub_issues: [
										{
											number: 2,
											title: "Child issue",
											state: "open",
											labels: [],
										},
									],
								},
							}),
						),
					),
				);
				await broker.flush();

				const triage = await controllerTriage.promise;
				expect(triage.payload).toMatchObject({ type: "triage", issue: root });

				const controllerExceptionTopic = `notifications.envoy.exceptions.notifications.role.${controller}`;
				broker.publish(
					controllerExceptionTopic,
					codec.encode(
						JSON.stringify(
							envelope(
								"controller-unclaimed",
								"envoy",
								controllerExceptionTopic,
								{
									original_topic: roleTopic(controller),
									event_id: "issue-opened",
									payload: JSON.stringify(triage.payload),
									reason: "no_holder",
								},
							),
						),
					),
				);
				await broker.flush();

				const controllerSpawnArgv = await controllerSpawn.promise;
				const controllerSecret = controllerSpawnArgv
					.find((argument) => argument.startsWith("LEGION_CONTROLLER_SECRET="))
					?.slice("LEGION_CONTROLLER_SECRET=".length);
				if (!controllerSecret)
					throw new Error(
						"Controller tmux spawn did not include its capability",
					);

				const daemonUrl = `http://127.0.0.1:${daemon.server.port}`;
				await post(`${daemonUrl}/legion/v1/admission`, {
					secret: controllerSecret,
					issue: root,
				});

				const rootSpawnArgv = await rootSpawn.promise;
				expect(rootSpawnArgv).toContain(
					`LEGION_DAEMON_URL=http://127.0.0.1:${daemonPort}`,
				);
				expect(rootSpawnArgv).toContain(`ENVOY_NATS_URL=${nats.url}`);

				const rootSessionFile = path.join(
					stateDir,
					"trees",
					"acme-widgets-1",
					".omp",
					"session.json",
				);
				await mkdir(path.dirname(rootSessionFile), { recursive: true });
				await writeFile(rootSessionFile, "{}\n", "utf8");
				const rootBootToken = rootSpawnArgv
					.find((argument) => argument.startsWith("LEGION_BOOT_TOKEN="))
					?.slice("LEGION_BOOT_TOKEN=".length);
				if (!rootBootToken)
					throw new Error("Root tmux spawn did not include its boot token");
				const started = await post(`${daemonUrl}/legion/v1/process/started`, {
					tree: root,
					generation: 1,
					bootToken: rootBootToken,
					rootSessionId: "root-session",
					ompSessionFile: rootSessionFile,
				});
				const rootStarted = (await started.json()) as {
					controlSubject: string;
					secret: string;
				};
				expect(rootStarted.controlSubject).toBe(
					`legion.ctl.${sanitizeToken(root)}.1`,
				);
				const architect = {
					sessionId: "root-session",
					secret: rootStarted.secret,
				};
				await post(`${daemonUrl}/legion/v1/process/ready`, {
					tree: root,
					...architect,
				});

				const architectSpawn = await post(
					`${daemonUrl}/legion/v1/spawn-token`,
					{
						tree: root,
						issue: child,
						role: "architect",
						...architect,
					},
				);
				const { spawnToken: architectSpawnToken } =
					(await architectSpawn.json()) as {
						spawnToken: string;
					};
				await post(`${daemonUrl}/legion/v1/role-backing`, {
					tree: root,
					issue: child,
					role: "architect",
					agentId: "child-architect",
					sessionId: "child-architect-session",
					spawnToken: architectSpawnToken,
				});
				const workerSpawn = await post(`${daemonUrl}/legion/v1/spawn-token`, {
					tree: root,
					issue: child,
					role: "implementer",
					...architect,
				});
				const { spawnToken: workerSpawnToken } = (await workerSpawn.json()) as {
					spawnToken: string;
				};
				await post(`${daemonUrl}/legion/v1/role-backing`, {
					tree: root,
					issue: child,
					role: "implementer",
					agentId: "child-worker",
					sessionId: "child-worker-session",
					spawnToken: workerSpawnToken,
				});
				await post(`${daemonUrl}/legion/v1/waves/release`, {
					tree: root,
					children: [child],
					...architect,
				});

				const childCommentTopic =
					"notifications.github.acme.widgets.issue.2.comment";
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
									html_url:
										"https://github.com/acme/widgets/issues/2#issuecomment-1",
								},
							}),
						),
					),
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
				expect(observedWorkerPublication.payload).toMatchObject({
					type: "work",
					issue: child,
				});

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
							}),
						),
					),
				);
				await broker.flush();

				const directive = await workerDirective.promise;
				expect(directive.subject).toBe(`legion.ctl.${sanitizeToken(root)}.1`);
				expect(directive.payload).toEqual({
					type: "revive-worker",
					issue: child,
					role: "implementer",
					agentId: "child-worker",
					parentSessionFile: rootSessionFile,
					redeliver: {
						topic: roleTopic(worker),
						payload: workerPayload,
						eventId: "worker-work",
					},
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
		},
		E2E_TEST_TIMEOUT_MS,
	);
});
