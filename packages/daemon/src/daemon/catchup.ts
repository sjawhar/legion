import {
	type IssueKey,
	type LegionRole,
	parseIssueKey,
	roleToken,
} from "@legion/contracts";
import type { CommandRunner, CommandRunnerOptions } from "../state/fetch";
import { buildRoleEnv, modeToRole, type TokenManager } from "./github-apps";
import type { LegionState, PrState, TreeState } from "./legion-state";
import type { LegionEventPayload } from "./reducers";

type JsonRecord = Record<string, unknown>;

type CiVerdict = "green" | "red" | "pending";
const WORKER_MODE: Record<LegionRole, string> = {
	architect: "architect",
	planner: "plan",
	implementer: "implement",
	tester: "test",
	reviewer: "review",
	merger: "merge",
};

export interface CatchupOverseerPayload extends LegionEventPayload {
	type: "catchup-overseer";
	gates: Record<IssueKey, { needsApproval: boolean; humanApproved: boolean }>;
	childCounts: Record<
		IssueKey,
		{ total: number; open: number; closed: number }
	>;
	prVerdicts: Record<
		string,
		{
			issue: IssueKey;
			sha: string;
			ci: CiVerdict;
			review: "approved" | "changes_requested" | "pending";
			fixAttempts: number;
		}
	>;
}

export type CatchupUnhandled =
	| {
			kind: "comment" | "review-comment";
			id: number;
			occurredAt: string;
			author: string;
			body: string;
			url: string;
	  }
	| {
			kind: "review";
			id: number;
			occurredAt: string;
			author: string;
			state: string;
			body: string;
			url: string;
	  }
	| { kind: "dispatch-reply"; thread: number; author: string; body: string };

export interface CatchupWorkerPayload extends LegionEventPayload {
	type: "catchup-worker";
	unhandled: CatchupUnhandled[];
}

export interface WorkerCatchupDeps {
	runner: CommandRunner;
	tokenManager: Pick<TokenManager, "getToken">;
}

interface Artifact {
	repo: `${string}/${string}`;
	number: number;
	isPullRequest: boolean;
}

function asRecord(value: unknown): JsonRecord | undefined {
	return typeof value === "object" && value !== null && !Array.isArray(value)
		? (value as JsonRecord)
		: undefined;
}

function stringValue(value: unknown): string | undefined {
	return typeof value === "string" ? value : undefined;
}

function numberValue(value: unknown): number | undefined {
	return typeof value === "number" && Number.isSafeInteger(value)
		? value
		: undefined;
}

function stateTree(state: LegionState, tree: IssueKey): Set<IssueKey> {
	if (!state.issues[tree]) throw new Error(`Unknown Legion tree: ${tree}`);
	const issues = new Set<IssueKey>();
	const pending = [tree];
	while (pending.length > 0) {
		const issue = pending.pop();
		if (!issue || issues.has(issue)) continue;
		issues.add(issue);
		for (const child of state.issues[issue]?.children ?? [])
			pending.push(child);
	}
	return issues;
}

function ciVerdict(pr: PrState): CiVerdict {
	const checks = Object.values(pr.checks);
	if (
		checks.some(
			(check) =>
				check.status === "completed" &&
				check.conclusion !== "success" &&
				check.conclusion !== "neutral" &&
				check.conclusion !== "skipped",
		)
	) {
		return "red";
	}
	return checks.length > 0 &&
		checks.every((check) => check.status === "completed")
		? "green"
		: "pending";
}

export async function overseerCatchup(
	s: LegionState,
	tree: IssueKey,
): Promise<LegionEventPayload> {
	const issues = stateTree(s, tree);
	const gates = {} as CatchupOverseerPayload["gates"];
	const childCounts = {} as CatchupOverseerPayload["childCounts"];
	for (const issue of [...issues].sort()) {
		const node = s.issues[issue];
		gates[issue] = {
			needsApproval: node.labels.includes("needs-approval"),
			humanApproved: node.labels.includes("human-approved"),
		};
		let open = 0;
		let closed = 0;
		for (const child of node.children) {
			if (s.issues[child]?.state === "open") open += 1;
			else if (s.issues[child]?.state === "closed") closed += 1;
		}
		childCounts[issue] = { total: node.children.length, open, closed };
	}

	const prVerdicts: CatchupOverseerPayload["prVerdicts"] = {};
	for (const [prKey, pr] of Object.entries(s.prs).sort(([first], [second]) =>
		first.localeCompare(second),
	)) {
		if (!issues.has(pr.key)) continue;
		prVerdicts[prKey] = {
			issue: pr.key,
			sha: pr.headSha,
			ci: ciVerdict(pr),
			review: pr.reviewDecision ?? "pending",
			fixAttempts: pr.fixAttempts,
		};
	}

	return { type: "catchup-overseer", gates, childCounts, prVerdicts };
}

function artifactsFor(state: LegionState, issue: IssueKey): Artifact[] {
	const prs = Object.values(state.prs)
		.filter((pr) => pr.key === issue)
		.sort((first, second) => first.number - second.number)
		.map((pr) => ({ repo: pr.repo, number: pr.number, isPullRequest: true }));
	if (prs.length > 0) return prs;
	const parsed = parseIssueKey(issue);
	if (!parsed) throw new Error(`Invalid IssueKey: ${issue}`);
	return [
		{
			repo: `${parsed.owner}/${parsed.repo}`,
			number: parsed.number,
			isPullRequest: false,
		},
	];
}

async function runJsonArray(
	runner: CommandRunner,
	command: string[],
	options: CommandRunnerOptions,
): Promise<JsonRecord[]> {
	const result = await runner(command, options);
	if (result.exitCode !== 0) {
		throw new Error(`GitHub catch-up query failed: ${result.stderr}`);
	}
	const parsed: unknown = JSON.parse(result.stdout);
	if (!Array.isArray(parsed)) {
		throw new Error("GitHub catch-up query returned an invalid timeline array");
	}
	const entries = (
		parsed.every(Array.isArray) ? parsed.flat() : parsed
	) as unknown[];
	if (entries.some((entry) => !asRecord(entry))) {
		throw new Error("GitHub catch-up query returned an invalid timeline array");
	}
	return entries as JsonRecord[];
}

function timestamp(value: unknown): number | undefined {
	const date = stringValue(value);
	if (!date) return undefined;
	const milliseconds = Date.parse(date);
	return Number.isNaN(milliseconds) ? undefined : milliseconds;
}

function authorLogin(entry: JsonRecord): string | undefined {
	return stringValue(asRecord(entry.user)?.login);
}

function isWorkerActivity(
	login: string | undefined,
	email: string | undefined,
	appLogin: string,
	appEmail: string,
): boolean {
	return login === appLogin || email === appEmail;
}

function isBot(login: string | undefined): boolean {
	return login?.endsWith("[bot]") ?? false;
}

function cursorFor(
	commits: JsonRecord[],
	comments: JsonRecord[],
	appLogin: string,
	appEmail: string,
): number | undefined {
	let cursor: number | undefined;
	for (const commit of commits) {
		const commitData = asRecord(commit.commit);
		const author = asRecord(commitData?.author);
		const committer = asRecord(commitData?.committer);
		for (const identity of [author, committer]) {
			if (
				!isWorkerActivity(
					undefined,
					stringValue(identity?.email),
					appLogin,
					appEmail,
				)
			)
				continue;
			const activityAt = timestamp(identity?.date);
			if (
				activityAt !== undefined &&
				(cursor === undefined || activityAt > cursor)
			)
				cursor = activityAt;
		}
	}
	for (const comment of comments) {
		if (!isWorkerActivity(authorLogin(comment), undefined, appLogin, appEmail))
			continue;
		const activityAt = timestamp(comment.created_at);
		if (
			activityAt !== undefined &&
			(cursor === undefined || activityAt > cursor)
		)
			cursor = activityAt;
	}
	return cursor;
}

function unhandledComments(
	comments: JsonRecord[],
	cursor: number | undefined,
	appLogin: string,
	kind: "comment" | "review-comment",
): CatchupUnhandled[] {
	const result: CatchupUnhandled[] = [];
	for (const comment of comments) {
		const author = authorLogin(comment);
		const occurredAt = stringValue(comment.created_at);
		const occurredAtMs = timestamp(occurredAt);
		const id = numberValue(comment.id);
		const body = stringValue(comment.body);
		const url = stringValue(comment.html_url);
		if (
			!author ||
			!occurredAt ||
			occurredAtMs === undefined ||
			id === undefined ||
			body === undefined ||
			!url ||
			isBot(author) ||
			author === appLogin ||
			(cursor !== undefined && occurredAtMs <= cursor)
		) {
			continue;
		}
		result.push({ kind, id, occurredAt, author, body, url });
	}
	return result;
}

function unhandledReviews(
	reviews: JsonRecord[],
	cursor: number | undefined,
	appLogin: string,
): CatchupUnhandled[] {
	const result: CatchupUnhandled[] = [];
	for (const review of reviews) {
		const author = authorLogin(review);
		const occurredAt = stringValue(review.submitted_at);
		const occurredAtMs = timestamp(occurredAt);
		const id = numberValue(review.id);
		const state = stringValue(review.state);
		const body = stringValue(review.body) ?? "";
		const url = stringValue(review.html_url);
		if (
			!author ||
			!occurredAt ||
			occurredAtMs === undefined ||
			id === undefined ||
			!state ||
			!url ||
			isBot(author) ||
			author === appLogin ||
			(cursor !== undefined && occurredAtMs <= cursor)
		) {
			continue;
		}
		result.push({
			kind: "review",
			id,
			occurredAt,
			author,
			state: state.toLowerCase(),
			body,
			url,
		});
	}
	return result;
}

function treeFor(state: LegionState, issue: IssueKey): TreeState | undefined {
	let current = issue;
	const visited = new Set<IssueKey>();
	while (!visited.has(current)) {
		visited.add(current);
		const tree = state.trees[current];
		if (tree) return tree;
		const parent = state.issues[current]?.parent;
		if (!parent) return undefined;
		current = parent;
	}
	return undefined;
}

function dispatchReplies(
	state: LegionState,
	issue: IssueKey,
	role: LegionRole,
): CatchupUnhandled[] {
	const tree = treeFor(state, issue);
	if (!tree) return [];
	const parsed = parseIssueKey(issue);
	if (!parsed) throw new Error(`Invalid IssueKey: ${issue}`);
	const repo = `${parsed.owner}/${parsed.repo}`;
	const token = roleToken(state.project, issue, role);
	const registeredThreads = new Set(
		state.dispatchThreads
			.filter((thread) => thread.repo === repo && thread.role === role)
			.map((thread) => thread.thread),
	);
	const result: CatchupUnhandled[] = [];
	for (const held of tree.heldEvents) {
		if (held.role !== token) continue;
		const payload = asRecord(JSON.parse(held.payloadJson));
		const thread = numberValue(payload?.thread);
		const author = stringValue(payload?.author);
		const body = stringValue(payload?.body);
		if (
			payload?.type !== "dispatch-reply" ||
			thread === undefined ||
			!registeredThreads.has(thread) ||
			!author ||
			body === undefined
		) {
			continue;
		}
		result.push({ kind: "dispatch-reply", thread, author, body });
	}
	return result;
}

export async function workerCatchup(
	s: LegionState,
	issue: IssueKey,
	role: LegionRole,
	deps: WorkerCatchupDeps,
): Promise<LegionEventPayload> {
	const parsedIssue = parseIssueKey(issue);
	if (!parsedIssue) throw new Error(`Invalid IssueKey: ${issue}`);
	const credential = await deps.tokenManager.getToken(
		modeToRole(WORKER_MODE[role]),
		parsedIssue.owner,
	);
	const options: CommandRunnerOptions = {
		env: buildRoleEnv(credential.token, credential.gitIdentity, process.env),
	};
	const unhandled: CatchupUnhandled[] = [];
	for (const artifact of artifactsFor(s, issue)) {
		const commits = artifact.isPullRequest
			? await runJsonArray(
					deps.runner,
					[
						"gh",
						"api",
						"--paginate",
						"--slurp",
						`repos/${artifact.repo}/pulls/${artifact.number}/commits`,
					],
					options,
				)
			: [];
		const comments = await runJsonArray(
			deps.runner,
			[
				"gh",
				"api",
				"--paginate",
				"--slurp",
				`repos/${artifact.repo}/issues/${artifact.number}/comments`,
			],
			options,
		);
		const reviewComments = artifact.isPullRequest
			? await runJsonArray(
					deps.runner,
					[
						"gh",
						"api",
						"--paginate",
						"--slurp",
						`repos/${artifact.repo}/pulls/${artifact.number}/comments`,
					],
					options,
				)
			: [];
		const cursor = cursorFor(
			commits,
			[...comments, ...reviewComments],
			credential.gitIdentity.name,
			credential.gitIdentity.email,
		);
		unhandled.push(
			...unhandledComments(
				comments,
				cursor,
				credential.gitIdentity.name,
				"comment",
			),
			...unhandledComments(
				reviewComments,
				cursor,
				credential.gitIdentity.name,
				"review-comment",
			),
		);
		if (artifact.isPullRequest) {
			const reviews = await runJsonArray(
				deps.runner,
				[
					"gh",
					"api",
					"--paginate",
					"--slurp",
					`repos/${artifact.repo}/pulls/${artifact.number}/reviews`,
				],
				options,
			);
			unhandled.push(
				...unhandledReviews(reviews, cursor, credential.gitIdentity.name),
			);
		}
	}
	unhandled.sort((first, second) => {
		const firstAt = "occurredAt" in first ? first.occurredAt : "";
		const secondAt = "occurredAt" in second ? second.occurredAt : "";
		return firstAt.localeCompare(secondAt);
	});
	unhandled.push(...dispatchReplies(s, issue, role));
	return { type: "catchup-worker", unhandled };
}
