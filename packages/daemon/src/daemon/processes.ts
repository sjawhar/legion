import { mkdir, stat, writeFile } from "node:fs/promises";
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
  workerCatchup: WorkerCatchupDeps;
  now(): number;
}

type RoleBacking = WorkerRoleClaim;

function treeName(issue: IssueKey): string {
  const parsed = parseIssueKey(issue);
  if (!parsed) throw new Error(`Invalid IssueKey: ${issue}`);
  return `${parsed.owner}-${parsed.repo}-${parsed.number}`;
}

function shellPath(value: string): string {
  return /[^A-Za-z0-9_./:-]/.test(value) ? `'${value.replaceAll("'", "'\\''")}'` : value;
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
  private readonly heldControllerEvents: Redelivery[] = [];
  private awaitingControllerReplacement = false;
  private controllerClaimBeforeReplacement: string | undefined;
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
    tree.status = "dead";
    this.releaseSlot(treeKey);
    await this.deps.saveState();
  }

  async spawnRoot(issue: IssueKey): Promise<void> {
    const tree = this.ensureTree(issue);
    const priorGeneration = tree.generation;
    const priorLocator = tree.locator;
    tree.generation += 1;
    try {
      await this.spawnTree(tree);
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
      this.awaitingControllerReplacement = true;
      this.controllerClaimBeforeReplacement =
        this.deps.state.roles[controllerToken(this.deps.state.project)]?.sessionId;
      this.controllerSpawn = (async () => {
        const controllerSecret = await this.deps.mintControllerCapability();
        await this.spawnController(controllerSecret);
      })().finally(() => {
        this.controllerSpawn = undefined;
      });
    }
    await this.controllerSpawn;
  }

  markControllerReady(): void {
    this.redeliverHeldControllerEvents();
  }

  async probe(treeKey: IssueKey): Promise<"alive" | "dead"> {
    const tree = this.deps.state.trees[treeKey];
    if (!tree?.locator) return "dead";

    const windows = await this.deps.run([
      "tmux",
      "list-windows",
      "-t",
      tree.locator.tmuxSession,
      "-F",
      "#{window_name}",
    ]);
    if (
      windows.exitCode !== 0 ||
      !windows.stdout.split(/\r?\n/).includes(tree.locator.tmuxWindow)
    ) {
      return "dead";
    }

    const panes = await this.deps.run([
      "tmux",
      "list-panes",
      "-t",
      `${tree.locator.tmuxSession}:${tree.locator.tmuxWindow}`,
      "-F",
      "#{pane_pid}",
    ]);
    const pid = Number(panes.stdout.trim().split(/\s+/)[0]);
    if (panes.exitCode !== 0 || !Number.isSafeInteger(pid) || pid <= 0) return "dead";

    const process = await this.deps.run(["kill", "-0", String(pid)]);
    return process.exitCode === 0 ? "alive" : "dead";
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
    this.releaseSlot(treeKey);
    this.persist();
  }

  expireLinger(treeKey: IssueKey): void {
    const tree = this.requireTree(treeKey);
    void this.controlDirective(treeKey, { type: "shutdown" }).catch((error) => {
      console.error(`[legion] shutdown directive failed for ${treeKey}:`, error);
    });
    if (tree.locator) {
      void this.deps.run([
        "tmux",
        "kill-window",
        "-t",
        `${tree.locator.tmuxSession}:${tree.locator.tmuxWindow}`,
      ]);
    }
    tree.status = "closed";
    delete tree.lingerUntil;
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if ("issue" in claim && this.rootForIssue(claim.issue) === treeKey) {
        delete this.deps.state.roles[token];
      }
    }
    this.persist();
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
      this.heldControllerEvents.push(exception.original);
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

  private async spawnTree(tree: TreeState): Promise<void> {
    const name = treeName(tree.root);
    const parsedTree = parseIssueKey(tree.root);
    if (!parsedTree) throw new Error(`Invalid IssueKey: ${tree.root}`);
    const workspace = await provisionIssueWorkspace(tree.root, {
      extensionPackage: EXTENSION_PACKAGE,
      stateDir: this.deps.config.stateDir,
      maxRecursionDepth: this.deps.config.maxRecursionDepth,
      provisioningToken: async () => await this.deps.provisioningToken(parsedTree.owner),
      run: async (command, options) => {
        const result = await this.deps.run(command, options);
        return { ...result, stderr: result.stderr ?? "" };
      },
    });
    const promptPath = path.join(EXTENSION_PACKAGE, "agents", "architect-root.md");
    await (this.deps.statPrompt ?? stat)(promptPath);
    const session = `legion-${this.deps.state.project}`;
    await this.ensureTmuxSession(session);
    const generation = tree.generation;
    const bootToken = await this.deps.mintBootToken(tree.root, generation);
    const window = name;
    const result = await this.deps.run([
      "tmux",
      "new-window",
      "-t",
      session,
      "-n",
      window,
      "-e",
      `LEGION_TREE=${tree.root}`,
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
      `PATH=${this.deps.panePath}`,
      `cd ${shellPath(workspace.workspaceDir)} && ${this.deps.ompInvocation} --extension ${shellPath(EXTENSION_PACKAGE)} --append-system-prompt "$(cat ${shellPath(promptPath)})"`,
    ]);
    if (result.exitCode !== 0) {
      throw new Error(`tmux new-window failed (exit ${result.exitCode}): ${result.stdout}`);
    }
    tree.locator = { tmuxSession: session, tmuxWindow: window };
    tree.status = "active";
  }

  private async spawnController(controllerSecret: string): Promise<void> {
    const controllerDir = path.join(this.deps.config.stateDir, "controller");
    const promptPath = path.join(EXTENSION_PACKAGE, "agents", "controller-root.md");
    await (this.deps.statPrompt ?? stat)(promptPath);
    await this.writeOmpConfig(controllerDir, this.deps.config.maxRecursionDepth);
    const session = `legion-${this.deps.state.project}`;
    await this.ensureTmuxSession(session);
    const result = await this.deps.run([
      "tmux",
      "new-window",
      "-t",
      session,
      "-n",
      "controller",
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
    if (result.exitCode !== 0) {
      throw new Error(`tmux new-window failed (exit ${result.exitCode}): ${result.stdout}`);
    }
  }

  private async writeOmpConfig(directory: string, maxRecursionDepth: number): Promise<void> {
    await mkdir(path.join(directory, ".omp"), { recursive: true });
    await writeFile(
      path.join(directory, ".omp", "config.yml"),
      `task:\n  maxRecursionDepth: ${maxRecursionDepth}\nextensions:\n  - ${EXTENSION_PACKAGE}\n`,
      "utf8"
    );
  }

  private async ensureTmuxSession(session: string): Promise<void> {
    const result = await this.deps.run(["tmux", "has-session", "-t", session]);
    if (result.exitCode !== 0) {
      const create = await this.deps.run(["tmux", "new-session", "-d", "-s", session]);
      if (create.exitCode !== 0) {
        throw new Error(`tmux new-session failed (exit ${create.exitCode}): ${create.stdout}`);
      }
    }
  }

  private async controllerAlive(): Promise<boolean> {
    const session = `legion-${this.deps.state.project}`;
    const windows = await this.deps.run([
      "tmux",
      "list-windows",
      "-t",
      session,
      "-F",
      "#{window_name}",
    ]);
    if (windows.exitCode !== 0 || !windows.stdout.split(/\r?\n/).includes("controller"))
      return false;
    const panes = await this.deps.run([
      "tmux",
      "list-panes",
      "-t",
      `${session}:controller`,
      "-F",
      "#{pane_pid}",
    ]);
    const pid = Number(panes.stdout.trim().split(/\s+/)[0]);
    if (panes.exitCode !== 0 || !Number.isSafeInteger(pid) || pid <= 0) return false;
    return (await this.deps.run(["kill", "-0", String(pid)])).exitCode === 0;
  }

  private async resurrectDeadTree(treeKey: IssueKey): Promise<void> {
    if ((await this.probe(treeKey)) === "alive") return;
    await this.spawnRoot(treeKey);
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

  private redeliverHeldControllerEvents(): void {
    const token = controllerToken(this.deps.state.project);
    const claim = this.deps.state.roles[token];
    if (!claim) return;
    if (this.awaitingControllerReplacement) {
      if (!claim.sessionId || claim.sessionId === this.controllerClaimBeforeReplacement) {
        return;
      }
      this.awaitingControllerReplacement = false;
      this.controllerClaimBeforeReplacement = undefined;
    }
    for (const event of this.heldControllerEvents.splice(0)) {
      this.deps.natsPublish(event.topic, event.payload);
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
