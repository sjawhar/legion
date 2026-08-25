import { createHash } from "node:crypto";
import { mkdir, readFile, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  controllerToken,
  type IssueKey,
  type LegionRole,
  parseIssueKey,
  parseRoleToken,
  roleToken,
  roleTopic,
  sanitizeToken,
} from "@legion/contracts";
import { provisionIssueWorkspace } from "@legion/workspace";
import type { CommandRunnerOptions } from "../state/fetch";
import { type WorkerCatchupDeps, workerCatchup } from "./catchup";
import type { DaemonConfig } from "./config";
import type { LegionState, TreeState, WorkerRoleClaim } from "./legion-state";

const HOUR_MS = 60 * 60 * 1000;

const MAX_LAUNCH_FAILURES = 3;
const EXTENSION_PACKAGE = path.resolve(import.meta.dir, "../../../envoy-omp-extension");
const DAEMON_CLI_ENTRYPOINT = path.resolve(import.meta.dir, "../cli/index.ts");

type Redelivery = { topic: string; payload: string; eventId: string };

export type ControlDirective =
  | {
      type: "revive-worker";
      issue: IssueKey;
      role: LegionRole;
      agentId: string;
      parentSessionFile: string;
      redeliver: Redelivery;
    }
  | { type: "reclaim-architect"; issue: IssueKey; redeliver: Redelivery }
  | { type: "shutdown" };

export interface ExceptionInfo {
  roleToken: string;
  reason: "no_holder" | "delivery_failed";
  original: Redelivery;
  nack?: { type: "revive-worker"; issue: IssueKey; role: LegionRole };
}

export interface ProcessManagerDeps {
  state: LegionState;
  saveState(): Promise<void>;
  config: DaemonConfig;
  ompInvocation: string;
  panePath: string;
  credentialHelper: string;
  run(
    cmd: string[],
    options?: CommandRunnerOptions
  ): Promise<{ stdout: string; stderr?: string; exitCode: number }>;
  natsPublish(subject: string, json: string): void;
  natsRequest(subject: string, json: string): Promise<string>;
  mintControllerCapability(): Promise<string>;
  mintBootToken(tree: IssueKey, generation: number): Promise<string>;
  provisioningToken(owner: string): Promise<string>;
  statPrompt?(promptPath: string): Promise<unknown>;
  readProcessCmdline?(pid: number): Promise<string>;
  workerCatchup: WorkerCatchupDeps;
  now(): number;
}

type RoleBacking = WorkerRoleClaim;

const MAX_TMUX_WINDOW_NAME_LENGTH = 160;

function treeName(issue: IssueKey): string {
  const parsed = parseIssueKey(issue);
  if (!parsed) throw new Error(`Invalid IssueKey: ${issue}`);
  const encodeIssuePart = (part: string) => {
    const normalized = part.toLowerCase();
    if (!/^[a-z0-9._-]+$/.test(normalized)) {
      throw new Error(`Invalid Legion issue token part: ${part}`);
    }
    return normalized.replaceAll("_", "_u").replaceAll(".", "_d").replaceAll("-", "_h");
  };
  const fullName = `${encodeIssuePart(parsed.owner)}__${encodeIssuePart(parsed.repo)}-${parsed.number}`;
  if (fullName.length <= MAX_TMUX_WINDOW_NAME_LENGTH) return fullName;

  const suffix = createHash("sha256").update(fullName).digest("hex").slice(0, 16);
  return `${fullName.slice(0, MAX_TMUX_WINDOW_NAME_LENGTH - suffix.length - 1)}-${suffix}`;
}

function shellPath(value: string): string {
  return /[^A-Za-z0-9_./:-]/.test(value) ? `'${value.replaceAll("'", "'\\''")}'` : value;
}
export function daemonCredentialHelper(
  runtime = process.execPath,
  entrypoint = DAEMON_CLI_ENTRYPOINT
): string {
  if (!path.isAbsolute(runtime) || !path.isAbsolute(entrypoint)) {
    throw new Error("Legion credential helper requires absolute runtime and CLI paths");
  }
  return `!${shellPath(runtime)} ${shellPath(entrypoint)} credential`;
}
function controlReplyType(raw: string): "ack" | "nack" {
  const payload: unknown = JSON.parse(raw);
  if (
    typeof payload !== "object" ||
    payload === null ||
    !("type" in payload) ||
    (payload.type !== "ack" && payload.type !== "nack")
  ) {
    throw new Error("Invalid Legion control directive reply");
  }
  return payload.type;
}

/** Starts and supervises only the tmux trees whose locator it records in Legion state. */
export class ProcessManager {
  private readonly resurrecting = new Map<IssueKey, Promise<void>>();
  private controllerSpawn?: Promise<void>;
  private promotionSweep?: Set<IssueKey>;

  constructor(private readonly deps: ProcessManagerDeps) {}

  admit(issue: IssueKey): "spawned" | "queued" {
    const tree = this.ensureTree(issue);
    const admission = this.deps.state.admission;
    admission.cap = this.deps.config.admissionCap;
    if (admission.active.includes(issue)) return "spawned";

    const queuedIndex = admission.queue.indexOf(issue);
    if (queuedIndex !== -1 || tree.status === "launch-failed") {
      if (queuedIndex !== -1) admission.queue.splice(queuedIndex, 1);
      if (tree.status === "launch-failed") tree.launchFailures = 0;
      tree.status = "queued";
      admission.queue.push(issue);
      this.beginPromotionSweep();
      return "queued";
    }

    if (admission.active.length >= admission.cap) {
      tree.status = "queued";
      admission.queue.push(issue);
      this.persist();
      return "queued";
    }

    admission.active.push(issue);
    tree.status = "active";
    this.persist();
    this.startRoot(issue);
    return "spawned";
  }

  releaseSlot(issue: IssueKey): void {
    const admission = this.deps.state.admission;
    const activeIndex = admission.active.indexOf(issue);
    if (activeIndex === -1) return;

    admission.active.splice(activeIndex, 1);
    this.beginPromotionSweep();
  }

  async registerRoleBacking(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    agentId: string
  ): Promise<void> {
    if (this.rootForIssue(issue) !== treeKey) {
      throw new Error(`Issue ${issue} does not belong to Legion tree ${treeKey}`);
    }
    const token = roleToken(this.deps.state.project, issue, role);
    const existing = this.deps.state.roles[token];
    this.deps.state.roles[token] = {
      issue,
      role,
      ...(existing?.sessionId ? { sessionId: existing.sessionId } : {}),
      agentId,
    };
    this.redeliverHeldRoleEvents(treeKey, issue, role);
    await this.deps.saveState();
  }

  async markProcessDead(treeKey: IssueKey, generation?: number): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (tree.generation !== (generation ?? tree.generation)) return;
    await this.removeTreeWindow(tree);
    tree.status = "dead";
    this.releaseSlot(treeKey);
    await this.deps.saveState();
  }

  async closeTree(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    await this.removeTreeWindow(tree);
    tree.status = "closed";
    delete tree.lingerUntil;
    this.releaseSlot(treeKey);
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if ("issue" in claim && this.rootForIssue(claim.issue) === treeKey) {
        delete this.deps.state.roles[token];
      }
    }
    this.clearTreePhases(treeKey);
    await this.deps.saveState();
  }

  async reconcileTmuxWindows(): Promise<void> {
    const session = `legion-${this.deps.state.project}`;
    const windows = await this.deps.run([
      "tmux",
      "list-windows",
      "-t",
      session,
      "-F",
      "#{window_id}",
    ]);
    if (windows.exitCode !== 0) return;

    const known = new Set(
      [
        ...Object.values(this.deps.state.trees).map((tree) => tree.locator?.tmuxWindowId),
        this.deps.state.controllerLocator?.tmuxWindowId,
      ].filter((windowId): windowId is string => windowId !== undefined)
    );
    for (const windowId of windows.stdout.split(/\r?\n/)) {
      if (!/^@\d+$/.test(windowId) || known.has(windowId)) continue;
      await this.deps.run(["tmux", "kill-window", "-t", windowId]);
    }
  }

  async spawnRoot(issue: IssueKey, resume = false, resumeSessionFile?: string): Promise<void> {
    const tree = this.ensureTree(issue);
    const priorGeneration = tree.generation;
    const priorLocator = tree.locator;
    tree.generation += 1;
    try {
      await this.spawnTree(tree, resume, resumeSessionFile);
      tree.launchFailures = 0;
      if (this.promotionSweep?.has(issue)) this.promotionSweep = undefined;
      await this.deps.saveState();
    } catch (error) {
      tree.generation = priorGeneration;
      if (priorLocator) tree.locator = priorLocator;
      else delete tree.locator;
      tree.launchFailures += 1;
      const activeIndex = this.deps.state.admission.active.indexOf(issue);
      if (activeIndex !== -1) this.deps.state.admission.active.splice(activeIndex, 1);
      if (tree.launchFailures >= MAX_LAUNCH_FAILURES) {
        tree.status = "launch-failed";
        const queueIndex = this.deps.state.admission.queue.indexOf(issue);
        if (queueIndex !== -1) this.deps.state.admission.queue.splice(queueIndex, 1);
        this.publishController({
          type: "launch-failed",
          issue,
          failures: tree.launchFailures,
        });
      } else {
        tree.status = "queued";
        if (!this.deps.state.admission.queue.includes(issue)) {
          this.deps.state.admission.queue.push(issue);
        }
      }
      if (this.promotionSweep) this.advancePromotionSweep();
      else this.beginPromotionSweep(issue);
      await this.deps.saveState();
      throw error;
    }
  }

  async ensureController(): Promise<void> {
    if (await this.controllerAlive()) return;
    if (!this.controllerSpawn) {
      this.controllerSpawn = (async () => {
        const controllerSecret = await this.deps.mintControllerCapability();
        await this.spawnController(controllerSecret);
      })().finally(() => {
        this.controllerSpawn = undefined;
      });
    }
    await this.controllerSpawn;
  }

  async probe(treeKey: IssueKey): Promise<"alive" | "dead"> {
    const tree = this.deps.state.trees[treeKey];
    const windowId = tree?.locator?.tmuxWindowId;
    if (!windowId) return "dead";

    const panes = await this.deps.run(["tmux", "list-panes", "-t", windowId, "-F", "#{pane_pid}"]);
    const pid = Number(panes.stdout.trim().split(/\s+/)[0]);
    if (panes.exitCode !== 0 || !Number.isSafeInteger(pid) || pid <= 0) return "dead";
    return (await this.isOmpPane(pid)) ? "alive" : "dead";
  }

  async controlDirective(
    tree: IssueKey,
    directive: ControlDirective,
    redeliver = true
  ): Promise<boolean> {
    const generation = this.deps.state.trees[tree]?.generation;
    if (generation === undefined) throw new Error(`Unknown Legion tree: ${tree}`);
    const reply = controlReplyType(
      await this.deps.natsRequest(
        `legion.ctl.${sanitizeToken(tree)}.${generation}`,
        JSON.stringify(directive)
      )
    );
    if (reply === "ack") {
      if (redeliver && "redeliver" in directive) {
        this.deps.natsPublish(directive.redeliver.topic, directive.redeliver.payload);
      }
      return true;
    }
    if (directive.type !== "shutdown") {
      this.publishController({
        type: "revive-failed",
        issue: directive.issue,
        role: directive.type === "revive-worker" ? directive.role : "architect",
      });
    }
    return false;
  }

  async resurrect(treeKey: IssueKey): Promise<void> {
    const current = this.resurrecting.get(treeKey);
    if (current) return current;

    const resurrection = this.resurrectDeadTree(treeKey).finally(() => {
      this.resurrecting.delete(treeKey);
    });
    this.resurrecting.set(treeKey, resurrection);
    return resurrection;
  }
  async markTreeReady(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    const recoveries = tree.recoveryEvents;
    if (!recoveries?.length) return;
    for (const recovery of [...recoveries]) {
      let recovered = false;
      if (recovery.issue === treeKey && recovery.role === "architect") {
        this.deps.natsPublish(recovery.original.topic, recovery.original.payload);
        recovered = true;
      } else {
        const backing = this.roleBacking(recovery.issue, recovery.role);
        if (backing?.agentId) {
          recovered = await this.reviveWorker(
            treeKey,
            recovery.issue,
            recovery.role,
            backing.agentId,
            recovery.original
          );
        }
      }
      if (recovered) recoveries.splice(recoveries.indexOf(recovery), 1);
    }
    await this.deps.saveState();
  }

  beginLinger(treeKey: IssueKey): void {
    const tree = this.requireTree(treeKey);
    tree.status = "lingering";
    tree.lingerUntil = new Date(
      this.deps.now() + this.deps.config.lingerHours * HOUR_MS
    ).toISOString();
    this.clearTreePhases(treeKey);
    this.releaseSlot(treeKey);
    this.persist();
  }

  async expireLinger(treeKey: IssueKey): Promise<void> {
    void this.controlDirective(treeKey, { type: "shutdown" }).catch((error) => {
      console.error(`[legion] shutdown directive failed for ${treeKey}:`, error);
    });
    await this.closeTree(treeKey);
  }

  async handleException(exception: ExceptionInfo): Promise<void> {
    if (exception.nack) {
      this.publishController({
        type: "revive-failed",
        issue: exception.nack.issue,
        role: exception.nack.role,
      });
      return;
    }

    const parsed = parseRoleToken(this.deps.state.project, exception.roleToken);
    if (!parsed) return;
    if ("controller" in parsed) {
      await this.ensureController();
      return;
    }

    const root = this.rootForIssue(parsed.issue);
    if (!root) throw new Error(`No Legion tree records issue ${parsed.issue}`);

    if (parsed.role === "architect" && parsed.issue === root) {
      if ((await this.probe(root)) === "alive") {
        await this.controlDirective(root, {
          type: "reclaim-architect",
          issue: parsed.issue,
          redeliver: exception.original,
        });
      } else {
        await this.persistRecovery(root, parsed.issue, parsed.role, exception.original);
        await this.resurrect(root);
      }
      return;
    }

    const backing = this.roleBacking(parsed.issue, parsed.role);
    if (!backing?.agentId) {
      this.hold(root, parsed.role, exception.original);
      await this.deps.saveState();
      return;
    }

    if ((await this.probe(root)) === "dead") {
      await this.persistRecovery(root, parsed.issue, parsed.role, exception.original);
      await this.resurrect(root);
      return;
    }

    await this.reviveWorker(root, parsed.issue, parsed.role, backing.agentId, exception.original);
  }

  private async reviveWorker(
    root: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    agentId: string,
    original: Redelivery
  ): Promise<boolean> {
    const sessionFile = this.requireTree(root).locator?.ompSessionFile;
    if (!sessionFile) throw new Error(`Live Legion tree ${root} has no OMP session file`);
    const revived = await this.controlDirective(
      root,
      {
        type: "revive-worker",
        issue,
        role,
        agentId,
        parentSessionFile: sessionFile,
        redeliver: original,
      },
      false
    );
    if (!revived) return false;
    const catchup = await workerCatchup(this.deps.state, issue, role, this.deps.workerCatchup);
    this.deps.natsPublish(
      roleTopic(roleToken(this.deps.state.project, issue, role)),
      JSON.stringify(catchup)
    );
    this.deps.natsPublish(original.topic, original.payload);
    return true;
  }

  private async persistRecovery(
    root: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    original: Redelivery
  ): Promise<void> {
    const tree = this.requireTree(root);
    let recoveries = tree.recoveryEvents;
    if (!recoveries) {
      recoveries = [];
      tree.recoveryEvents = recoveries;
    }
    if (
      !recoveries.some(
        (recovery) =>
          recovery.issue === issue &&
          recovery.role === role &&
          recovery.original.eventId === original.eventId
      )
    ) {
      recoveries.push({ issue, role, original });
    }
    await this.deps.saveState();
  }

  private startRoot(issue: IssueKey): void {
    void this.spawnRoot(issue).catch((error) => {
      console.error(`[legion] failed to spawn ${issue}:`, error);
    });
  }

  private beginPromotionSweep(initialFailure?: IssueKey): void {
    if (this.promotionSweep) {
      this.advancePromotionSweep();
      return;
    }
    this.promotionSweep = new Set(initialFailure ? [initialFailure] : []);
    this.advancePromotionSweep();
  }

  private advancePromotionSweep(): void {
    const sweep = this.promotionSweep;
    if (!sweep) return;
    if (this.deps.state.admission.active.length >= this.deps.state.admission.cap) {
      this.promotionSweep = undefined;
      this.persist();
      return;
    }
    const queue = this.deps.state.admission.queue;
    const nextIndex = queue.findIndex((candidate) => {
      const tree = this.ensureTree(candidate);
      return !sweep.has(candidate) && tree.status !== "launch-failed";
    });
    if (nextIndex === -1) {
      this.promotionSweep = undefined;
      this.persist();
      return;
    }
    const [next] = queue.splice(nextIndex, 1);
    sweep.add(next);
    this.deps.state.admission.active.push(next);
    this.ensureTree(next).status = "active";
    this.persist();
    this.startRoot(next);
  }

  private ensureTree(issue: IssueKey): TreeState {
    const existing = this.deps.state.trees[issue];
    if (existing) return existing;
    const tree: TreeState = {
      root: issue,
      generation: 0,
      status: "queued",
      launchFailures: 0,
      heldEvents: [],
    };
    this.deps.state.trees[issue] = tree;
    return tree;
  }

  private requireTree(treeKey: IssueKey): TreeState {
    const tree = this.deps.state.trees[treeKey];
    if (!tree) throw new Error(`Unknown Legion tree: ${treeKey}`);
    return tree;
  }

  private async spawnTree(
    tree: TreeState,
    resume: boolean,
    resumeSessionFile?: string
  ): Promise<void> {
    const name = treeName(tree.root);
    const parsedTree = parseIssueKey(tree.root);
    if (!parsedTree) throw new Error(`Invalid IssueKey: ${tree.root}`);
    const workspace = await provisionIssueWorkspace(tree.root, {
      extensionPackage: EXTENSION_PACKAGE,
      stateDir: this.deps.config.stateDir,
      maxRecursionDepth: this.deps.config.maxRecursionDepth,
      provisioningToken: async () => await this.deps.provisioningToken(parsedTree.owner),
      credentialHelper: this.deps.credentialHelper,
      run: async (command, options) => {
        const result = await this.deps.run(command, options);
        return { ...result, stderr: result.stderr ?? "" };
      },
    });
    const promptPath = path.join(EXTENSION_PACKAGE, "agents", "architect-root.md");
    await (this.deps.statPrompt ?? stat)(promptPath);
    const priorSessionFile = resume
      ? (resumeSessionFile ?? tree.locator?.ompSessionFile)
      : undefined;
    let resumeArgument = "";
    if (priorSessionFile) {
      try {
        await stat(priorSessionFile);
        resumeArgument = ` --resume=${shellPath(priorSessionFile)}`;
        console.info(
          `[legion] resurrecting ${tree.root} by resuming OMP session ${priorSessionFile}`
        );
      } catch (error) {
        if (
          typeof error !== "object" ||
          error === null ||
          !("code" in error) ||
          error.code !== "ENOENT"
        ) {
          throw error;
        }
        console.info(
          `[legion] resurrecting ${tree.root} with a fresh OMP session; recorded session file is missing: ${priorSessionFile}`
        );
      }
    }
    const session = `legion-${this.deps.state.project}`;
    const generation = tree.generation;
    const bootToken = await this.deps.mintBootToken(tree.root, generation);
    const tmuxWindowId = await this.openTmuxWindow(session, name, [
      "-e",
      `LEGION_TREE=${tree.root}`,
      "-e",
      `LEGION_ROOT_WORKSPACE=${workspace.workspaceDir}`,
      "-e",
      `LEGION_GENERATION=${generation}`,
      "-e",
      `LEGION_BOOT_TOKEN=${bootToken}`,
      "-e",
      `LEGION_DAEMON_URL=http://127.0.0.1:${this.deps.config.port}`,
      "-e",
      `LEGION_PROJECT=${this.deps.state.project}`,
      "-e",
      `ENVOY_NATS_URL=${this.deps.config.natsUrls.join(",")}`,
      "-e",
      `ENVOY_URL=${this.deps.config.envoyUrl}`,
      "-e",
      `LEGION_CONTROL_SUBJECT=legion.ctl.${sanitizeToken(tree.root)}.${generation}`,
      "-e",
      `LEGION_WORKER_BUDGET=${this.deps.config.workerBudget}`,
      "-e",
      `LEGION_MAX_RECURSION_DEPTH=${this.deps.config.maxRecursionDepth}`,
      "-e",
      `LEGION_STATE_DIR=${this.deps.config.stateDir}`,
      "-e",
      `LEGION_CREDENTIAL_HELPER=${this.deps.credentialHelper}`,
      "-e",
      "GIT_CONFIG_COUNT=0",
      "-e",
      "GIT_TERMINAL_PROMPT=0",
      "-e",
      `PATH=${this.deps.panePath}`,
      `cd ${shellPath(workspace.workspaceDir)} && ${this.deps.ompInvocation}${resumeArgument} --extension ${shellPath(EXTENSION_PACKAGE)} --append-system-prompt "$(cat ${shellPath(promptPath)})"`,
    ]);
    tree.locator = { tmuxSession: session, tmuxWindowId };
    tree.status = "active";
  }

  private async spawnController(controllerSecret: string): Promise<void> {
    const controllerDir = path.join(this.deps.config.stateDir, "controller");
    const promptPath = path.join(EXTENSION_PACKAGE, "agents", "controller-root.md");
    await (this.deps.statPrompt ?? stat)(promptPath);
    await this.writeOmpConfig(controllerDir, this.deps.config.maxRecursionDepth);
    const session = `legion-${this.deps.state.project}`;
    const tmuxWindowId = await this.openTmuxWindow(session, "controller", [
      "-e",
      "LEGION_CONTROLLER=1",
      "-e",
      `LEGION_CONTROLLER_SECRET=${controllerSecret}`,
      "-e",
      `LEGION_DAEMON_URL=http://127.0.0.1:${this.deps.config.port}`,
      "-e",
      `LEGION_PROJECT=${this.deps.state.project}`,
      "-e",
      `ENVOY_NATS_URL=${this.deps.config.natsUrls.join(",")}`,
      "-e",
      `ENVOY_URL=${this.deps.config.envoyUrl}`,
      "-e",
      `PATH=${this.deps.panePath}`,
      `cd ${shellPath(controllerDir)} && ${this.deps.ompInvocation} --extension ${shellPath(EXTENSION_PACKAGE)} --append-system-prompt "$(cat ${shellPath(promptPath)})"`,
    ]);
    this.deps.state.controllerLocator = { tmuxSession: session, tmuxWindowId };
    await this.deps.saveState();
  }

  private async writeOmpConfig(directory: string, maxRecursionDepth: number): Promise<void> {
    await mkdir(path.join(directory, ".omp"), { recursive: true });
    await writeFile(
      path.join(directory, ".omp", "config.yml"),
      `task:\n  maxRecursionDepth: ${maxRecursionDepth}\nextensions:\n  - ${EXTENSION_PACKAGE}\n`,
      "utf8"
    );
  }

  private async openTmuxWindow(
    session: string,
    name: string,
    environmentAndCommand: string[]
  ): Promise<string> {
    const sessionExists =
      (await this.deps.run(["tmux", "has-session", "-t", session])).exitCode === 0;
    const bootstrapWindow = "__legion_bootstrap";
    if (!sessionExists) {
      const create = await this.deps.run([
        "tmux",
        "new-session",
        "-d",
        "-s",
        session,
        "-n",
        bootstrapWindow,
        "sleep 3600",
      ]);
      if (create.exitCode !== 0) {
        throw new Error(`tmux new-session failed (exit ${create.exitCode}): ${create.stdout}`);
      }
      const marker = await this.deps.run([
        "tmux",
        "set-option",
        "-t",
        session,
        "@legion_owner",
        `legion-${this.deps.state.project}`,
      ]);
      if (marker.exitCode !== 0) {
        throw new Error(`tmux ownership marker failed (exit ${marker.exitCode}): ${marker.stdout}`);
      }
    }

    const command = [
      "tmux",
      "new-window",
      "-P",
      "-F",
      "#{window_id}",
      "-t",
      session,
      "-n",
      name,
      ...environmentAndCommand,
    ];
    const result = await this.deps.run(command);
    if (!sessionExists) {
      const cleanup = await this.deps.run([
        "tmux",
        "kill-window",
        "-t",
        `${session}:${bootstrapWindow}`,
      ]);
      if (cleanup.exitCode !== 0) {
        throw new Error(
          `tmux bootstrap window cleanup failed (exit ${cleanup.exitCode}): ${cleanup.stdout}`
        );
      }
    }
    if (result.exitCode !== 0) {
      throw new Error(`tmux new-window failed (exit ${result.exitCode}): ${result.stdout}`);
    }
    const tmuxWindowId = result.stdout.trim();
    if (!/^@\d+$/.test(tmuxWindowId)) {
      throw new Error(`tmux new-window did not report a window id: ${result.stdout}`);
    }
    return tmuxWindowId;
  }

  private async removeTreeWindow(tree: TreeState): Promise<void> {
    const windowId = tree.locator?.tmuxWindowId;
    delete tree.locator;
    if (!windowId) return;
    await this.deps.run(["tmux", "kill-window", "-t", windowId]);
  }

  private async isOmpPane(pid: number): Promise<boolean> {
    if ((await this.deps.run(["kill", "-0", String(pid)])).exitCode !== 0) return false;
    try {
      const cmdline = this.deps.readProcessCmdline
        ? await this.deps.readProcessCmdline(pid)
        : await readFile(`/proc/${pid}/cmdline`, "utf8");
      return cmdline.includes("omp");
    } catch {
      return false;
    }
  }

  private async controllerAlive(): Promise<boolean> {
    const windowId = this.deps.state.controllerLocator?.tmuxWindowId;
    if (!windowId) return false;
    const panes = await this.deps.run(["tmux", "list-panes", "-t", windowId, "-F", "#{pane_pid}"]);
    const pid = Number(panes.stdout.trim().split(/\s+/)[0]);
    if (panes.exitCode !== 0 || !Number.isSafeInteger(pid) || pid <= 0) {
      delete this.deps.state.controllerLocator;
      return false;
    }
    const alive = await this.isOmpPane(pid);
    if (!alive) delete this.deps.state.controllerLocator;
    return alive;
  }

  private async resurrectDeadTree(treeKey: IssueKey): Promise<void> {
    if ((await this.probe(treeKey)) === "alive") return;
    const tree = this.requireTree(treeKey);
    const resumeSessionFile = tree.locator?.ompSessionFile;
    await this.removeTreeWindow(tree);
    tree.status = "dead";
    await this.spawnRoot(treeKey, true, resumeSessionFile);
  }

  private rootForIssue(issue: IssueKey): IssueKey | undefined {
    if (this.deps.state.trees[issue]) return issue;
    const seen = new Set<IssueKey>();
    let current: IssueKey | undefined = issue;
    while (current && !seen.has(current)) {
      if (this.deps.state.trees[current]) return current;
      seen.add(current);
      current = this.deps.state.issues[current]?.parent;
    }
    return undefined;
  }

  private clearTreePhases(treeKey: IssueKey): void {
    for (const issue of Object.keys(this.deps.state.phases) as IssueKey[]) {
      if (this.rootForIssue(issue) === treeKey) delete this.deps.state.phases[issue];
    }
  }

  private roleBacking(issue: IssueKey, role: LegionRole): RoleBacking | undefined {
    for (const claim of Object.values(this.deps.state.roles) as RoleBacking[]) {
      if (claim.issue === issue && claim.role === role) return claim;
    }
    return undefined;
  }

  private hold(root: IssueKey, role: LegionRole, event: Redelivery): void {
    this.requireTree(root).heldEvents.push({
      role,
      payloadJson: event.payload,
      heldAt: new Date(this.deps.now()).toISOString(),
      eventId: event.eventId,
    });
  }
  private redeliverHeldRoleEvents(treeKey: IssueKey, issue: IssueKey, role: LegionRole): void {
    if (this.deps.state.issues[issue]?.released !== true) return;
    const token = roleToken(this.deps.state.project, issue, role);
    const heldEvents = this.requireTree(treeKey).heldEvents;
    for (let index = 0; index < heldEvents.length; ) {
      const held = heldEvents[index];
      if (held?.role !== role) {
        index += 1;
        continue;
      }
      this.deps.natsPublish(roleTopic(token), held.payloadJson);
      heldEvents.splice(index, 1);
    }
  }

  private publishController(
    payload:
      | { type: "revive-failed"; issue: IssueKey; role: LegionRole }
      | { type: "launch-failed"; issue: IssueKey; failures: number }
  ): void {
    this.deps.natsPublish(
      roleTopic(controllerToken(this.deps.state.project)),
      JSON.stringify(payload)
    );
  }

  private persist(): void {
    void this.deps.saveState();
  }
}
