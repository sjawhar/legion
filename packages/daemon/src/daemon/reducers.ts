import {
	formatIssueKey,
	type IssueKey,
	LEGION_ROLES,
	roleToken,
} from "@legion/contracts";
import type {
	IssueNode,
	LegionState,
	PrState,
	TreeState,
} from "./legion-state";

export interface LegionEventPayload {
	type: string;
	[key: string]: unknown;
}

export type Effect =
	| { kind: "publish"; role: string; payload: LegionEventPayload }
	| { kind: "hold"; tree: IssueKey; role: string; payload: LegionEventPayload }
	| { kind: "controller"; payload: LegionEventPayload }
	| { kind: "probe"; tree: IssueKey }
	| { kind: "linger"; tree: IssueKey }
	| { kind: "approval-status"; repo: string; pr: number; sha: string };

export interface EnvelopeJson {
	event_id: string;
	issued_at: number;
	payload?: string | Record<string, unknown>;
	payload_summary?: string;
	[key: string]: unknown;
}

export interface ReducerConfig {
	boardProjectIds: readonly string[];
	appLogins: readonly string[];
	maxFixAttempts: number;
}

export type CiEmission =
	| { type: "ci-green"; sha: string }
	| { type: "ci-settled-red"; sha: string; failing: string[] };

type JsonRecord = Record<string, unknown>;
type RoutedRole = "architect" | "implementer";

const SURVIVING_LABELS: Record<string, true> = {
	"needs-approval": true,
	"human-approved": true,
	"legion-child": true,
	"legion-backlog": true,
};

function asRecord(value: unknown): JsonRecord | undefined {
	return typeof value === "object" && value !== null && !Array.isArray(value)
		? (value as JsonRecord)
		: undefined;
}

function stringValue(value: unknown): string | undefined {
	return typeof value === "string" ? value : undefined;
}

function numberValue(value: unknown): number | undefined {
	if (typeof value === "number" && Number.isSafeInteger(value)) return value;
	if (typeof value === "string" && /^\d+$/.test(value)) {
		const parsed = Number(value);
		return Number.isSafeInteger(parsed) ? parsed : undefined;
	}
	return undefined;
}

function payloadFrom(envelope: EnvelopeJson): JsonRecord | undefined {
	if (typeof envelope.payload !== "string") return asRecord(envelope.payload);
	try {
		return asRecord(JSON.parse(envelope.payload));
	} catch {
		return undefined;
	}
}

function repository(payload: JsonRecord): string | undefined {
	return (
		stringValue(asRecord(payload.repository)?.full_name) ??
		stringValue(payload.repo)
	);
}

function keyFor(repo: string, number: number): IssueKey | undefined {
	const [owner, name, ...extra] = repo.split("/");
	if (!owner || !name || extra.length > 0) return undefined;
	return formatIssueKey(owner, name, number);
}

function labels(value: unknown): string[] {
	const source = Array.isArray(value) ? value : asRecord(value)?.nodes;
	if (!Array.isArray(source)) return [];
	const result: string[] = [];
	for (const label of source) {
		const name =
			typeof label === "string" ? label : stringValue(asRecord(label)?.name);
		if (name && SURVIVING_LABELS[name] && !result.includes(name))
			result.push(name);
	}
	return result;
}

function childKeys(repo: string, raw: JsonRecord): IssueKey[] {
	const subIssues = raw.sub_issues;
	const values = Array.isArray(subIssues)
		? subIssues
		: asRecord(subIssues)?.nodes;
	if (!Array.isArray(values)) return [];
	const result: IssueKey[] = [];
	for (const value of values) {
		const number = numberValue(asRecord(value)?.number);
		const key = number === undefined ? undefined : keyFor(repo, number);
		if (key && !result.includes(key)) result.push(key);
	}
	return result;
}

function treeFor(state: LegionState, key: IssueKey): TreeState | undefined {
	let current = key;
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

function hold(
	tree: TreeState,
	role: string,
	payload: LegionEventPayload,
	envelope: EnvelopeJson,
): void {
	tree.heldEvents.push({
		role,
		payloadJson: JSON.stringify(payload),
		heldAt: new Date(envelope.issued_at).toISOString(),
		eventId: envelope.event_id,
	});
}

function routeToken(
	state: LegionState,
	issue: IssueKey,
	token: string,
	payload: LegionEventPayload,
	envelope: EnvelopeJson,
): Effect[] {
	const node = state.issues[issue];
	const tree = treeFor(state, issue);
	if (!node || !tree) return [];
	if (node.released && tree.status !== "closed" && state.roles[token]) {
		return [{ kind: "publish", role: token, payload }];
	}
	hold(tree, token, payload, envelope);
	return [{ kind: "hold", tree: tree.root, role: token, payload }];
}

function route(
	state: LegionState,
	issue: IssueKey,
	role: RoutedRole,
	payload: LegionEventPayload,
	envelope: EnvelopeJson,
): Effect[] {
	return routeToken(
		state,
		issue,
		roleToken(state.project, issue, role),
		payload,
		envelope,
	);
}

function openChildren(state: LegionState, parent: IssueNode): number {
	return parent.children.filter((key) => state.issues[key]?.state === "open")
		.length;
}

function boardEvent(payload: JsonRecord, config: ReducerConfig): boolean {
	const project = asRecord(payload.project);
	const item = asRecord(payload.projects_v2_item);
	const id =
		stringValue(project?.id) ??
		stringValue(payload.project_node_id) ??
		stringValue(item?.project_node_id) ??
		stringValue(asRecord(item?.project)?.id);
	return id !== undefined && config.boardProjectIds.includes(id);
}

function addNode(
	state: LegionState,
	key: IssueKey,
	raw: JsonRecord,
	released: boolean,
	parent?: IssueKey,
): IssueNode {
	const prior = state.issues[key];
	const node: IssueNode = {
		key,
		title: stringValue(raw.title) ?? prior?.title ?? key,
		state: stringValue(raw.state) === "closed" ? "closed" : "open",
		children: prior?.children ?? [],
		released: prior?.released ?? released,
		labels: labels(raw.labels),
		...(prior?.finalCommentRef
			? { finalCommentRef: prior.finalCommentRef }
			: {}),
	};
	const ancestor = parent ?? prior?.parent;
	if (ancestor) node.parent = ancestor;
	state.issues[key] = node;
	return node;
}

function addAttribution(
	state: LegionState,
	entry: {
		sha?: string;
		commentId?: number;
		sessionId: string;
		issue: IssueKey;
		phase: string;
	},
): void {
	if (
		state.attribution.some(
			(current) =>
				current.sha === entry.sha &&
				current.commentId === entry.commentId &&
				current.sessionId === entry.sessionId,
		)
	) {
		return;
	}
	state.attribution.push(entry);
}

function footer(
	body: string,
): { sessionId: string; phase?: string } | undefined {
	const match = /<!--\s*legion:\s*({[\s\S]*?})\s*-->/.exec(body);
	if (!match) return undefined;
	try {
		const parsed = asRecord(JSON.parse(match[1]));
		const sessionId =
			stringValue(parsed?.session) ??
			stringValue(parsed?.sessionId) ??
			stringValue(parsed?.session_id);
		return sessionId
			? { sessionId, phase: stringValue(parsed?.phase) }
			: undefined;
	} catch {
		return undefined;
	}
}

function sessionForCommit(commit: JsonRecord): string | undefined {
	const message = stringValue(commit.message) ?? "";
	const trailer = /(?:^|\n)Legion-Session:\s*([^\s]+)/im.exec(message)?.[1];
	if (trailer) return trailer;
	const email =
		stringValue(asRecord(commit.author)?.email) ??
		stringValue(commit.author_email) ??
		"";
	return /\+([^@+\s]+)@/.exec(email)?.[1];
}

function indexPush(
	state: LegionState,
	payload: JsonRecord,
	repo: string,
): void {
	const branch = /^refs\/heads\/legion\/issue-(\d+)$/.exec(
		stringValue(payload.ref) ?? "",
	);
	if (!branch) return;
	const issue = keyFor(repo, Number(branch[1]));
	if (!issue || !Array.isArray(payload.commits)) return;
	for (const value of payload.commits) {
		const commit = asRecord(value);
		if (!commit) continue;
		const sessionId = sessionForCommit(commit);
		const sha = stringValue(commit.id) ?? stringValue(commit.sha);
		if (!sessionId || !sha) continue;
		addAttribution(state, {
			sha,
			sessionId,
			issue,
			phase: state.phases[issue]?.phase ?? "implement",
		});
	}
}

function indexComment(
	state: LegionState,
	payload: JsonRecord,
	repo: string,
): void {
	const rawIssue = asRecord(payload.issue);
	const comment = asRecord(payload.comment);
	const number = numberValue(rawIssue?.number);
	const parsed = footer(stringValue(comment?.body) ?? "");
	const issue = number === undefined ? undefined : keyFor(repo, number);
	const commentId = numberValue(comment?.id);
	if (!parsed || !issue || commentId === undefined) return;
	addAttribution(state, {
		commentId,
		sessionId: parsed.sessionId,
		issue,
		phase: parsed.phase ?? state.phases[issue]?.phase ?? "comment",
	});
}

function filtered(comment: JsonRecord, config: ReducerConfig): boolean {
	const author =
		stringValue(asRecord(comment.user)?.login) ?? stringValue(comment.author);
	return (
		(stringValue(comment.body) ?? "").includes("<!-- legion:") ||
		(author !== undefined && config.appLogins.includes(author))
	);
}

function isFailingCheck(check: {
	status: string;
	conclusion: string | null;
}): boolean {
	return (
		check.status === "completed" &&
		check.conclusion !== "success" &&
		check.conclusion !== "neutral" &&
		check.conclusion !== "skipped"
	);
}

function isGreen(pr: PrState): boolean {
	const checks = Object.values(pr.checks);
	return (
		checks.length > 0 &&
		checks.every(
			(check) => check.status === "completed" && !isFailingCheck(check),
		)
	);
}

function wasRed(pr: PrState): boolean {
	return Object.values(pr.checks).some(isFailingCheck);
}

function issueForBranch(repo: string, branch: string): IssueKey | undefined {
	const match = /^legion\/issue-(\d+)$/.exec(branch);
	return match ? keyFor(repo, Number(match[1])) : undefined;
}

function removeBranchMappings(state: LegionState, prKey: string): void {
	for (const [branch, mapped] of Object.entries(state.prByBranch)) {
		if (mapped === prKey) delete state.prByBranch[branch];
	}
}

function ingress(
	state: LegionState,
	payload: JsonRecord,
	config: ReducerConfig,
): Effect[] | undefined {
	const raw =
		asRecord(payload.issue) ??
		asRecord(asRecord(payload.projects_v2_item)?.content);
	const action = stringValue(payload.action);
	if (
		!raw ||
		!boardEvent(payload, config) ||
		(action !== "opened" && action !== "created")
	)
		return undefined;
	const repo = repository(payload);
	const number = numberValue(raw.number);
	if (!repo || number === undefined) return [];
	const key = keyFor(repo, number);
	if (!key) return [];
	const currentLabels = labels(raw.labels);
	if (
		currentLabels.includes("legion-child") ||
		currentLabels.includes("legion-backlog")
	)
		return [];
	const preexistingChildren = childKeys(repo, raw);
	addNode(state, key, raw, true).children = preexistingChildren;
	const rawSubIssues = raw.sub_issues;
	const values = Array.isArray(rawSubIssues)
		? rawSubIssues
		: asRecord(rawSubIssues)?.nodes;
	if (Array.isArray(values)) {
		for (const value of values) {
			const child = asRecord(value);
			const childNumber = numberValue(child?.number);
			const childKey =
				childNumber === undefined ? undefined : keyFor(repo, childNumber);
			if (child && childKey) addNode(state, childKey, child, false, key);
		}
	}
	return [
		{
			kind: "controller",
			payload: { type: "triage", issue: key, preexistingChildren },
		},
	];
}

function subIssue(
	state: LegionState,
	payload: JsonRecord,
	envelope: EnvelopeJson,
): Effect[] | undefined {
	const rawParent = asRecord(payload.parent_issue);
	const rawChild = asRecord(payload.sub_issue);
	if (!rawParent || !rawChild) return undefined;
	const repo = repository(payload);
	const parentNumber = numberValue(rawParent.number);
	const childNumber = numberValue(rawChild.number);
	if (!repo || parentNumber === undefined || childNumber === undefined)
		return [];
	const parentKey = keyFor(repo, parentNumber);
	const childKey = keyFor(repo, childNumber);
	const parent = parentKey ? state.issues[parentKey] : undefined;
	if (!parentKey || !childKey || !parent) return [];

	if (payload.action === "sub_issue_added") {
		const known = parent.children.includes(childKey);
		if (!known) parent.children.push(childKey);
		const child =
			state.issues[childKey] ??
			addNode(state, childKey, rawChild, false, parentKey);
		child.parent = parentKey;
		if (known || treeFor(state, parentKey)?.status !== "active") return [];
		return route(
			state,
			parentKey,
			"architect",
			{
				type: "child-adopted",
				child: childKey,
				remaining: openChildren(state, parent),
			},
			envelope,
		);
	}

	if (
		payload.action !== "sub_issue_removed" ||
		!parent.children.includes(childKey)
	)
		return [];
	const child = state.issues[childKey];
	const wasOpen = child?.state === "open";
	parent.children = parent.children.filter((key) => key !== childKey);
	if (child?.parent === parentKey) delete child.parent;
	const result = route(
		state,
		parentKey,
		"architect",
		{
			type: "child-removed",
			child: childKey,
			remaining: openChildren(state, parent),
		},
		envelope,
	);
	if (wasOpen && openChildren(state, parent) === 0) {
		result.push(
			...route(
				state,
				parentKey,
				"architect",
				{ type: "children-complete" },
				envelope,
			),
		);
	}
	return result;
}

function issueEvent(
	state: LegionState,
	payload: JsonRecord,
	envelope: EnvelopeJson,
): Effect[] | undefined {
	const raw = asRecord(payload.issue);
	if (!raw || payload.comment !== undefined || raw.pull_request !== undefined)
		return undefined;
	const repo = repository(payload);
	const number = numberValue(raw.number);
	const key = repo && number !== undefined ? keyFor(repo, number) : undefined;
	const node = key ? state.issues[key] : undefined;
	if (!key || !node) return [];

	if (payload.action === "labeled" || payload.action === "unlabeled") {
		const label = stringValue(asRecord(payload.label)?.name);
		if (!label || !SURVIVING_LABELS[label]) return [];
		if (payload.action === "labeled") {
			if (node.labels.includes(label)) return [];
			node.labels.push(label);
			return label === "human-approved"
				? route(state, key, "architect", { type: "human-approved" }, envelope)
				: [];
		}
		if (!node.labels.includes(label)) return [];
		node.labels = node.labels.filter((value) => value !== label);
		return [];
	}

	if (payload.action === "closed") {
		const parent = node.parent ? state.issues[node.parent] : undefined;
		const wasOpen = node.state === "open";
		node.state = "closed";
		if (!parent || !node.parent) {
			return state.trees[key]?.status === "active"
				? [{ kind: "linger", tree: key }]
				: [];
		}
		if (!wasOpen) return [];
		const result = route(
			state,
			node.parent,
			"architect",
			{
				type: "child-closed",
				child: key,
				completion: stringValue(raw.state_reason) ?? "closed",
				remaining: openChildren(state, parent),
				finalCommentRef:
					stringValue(raw.final_comment_ref) ??
					stringValue(payload.final_comment_ref) ??
					node.finalCommentRef ??
					null,
			},
			envelope,
		);
		if (openChildren(state, parent) === 0) {
			result.push(
				...route(
					state,
					node.parent,
					"architect",
					{ type: "children-complete" },
					envelope,
				),
			);
		}
		return result;
	}

	if (payload.action !== "reopened") return [];
	node.state = "open";
	delete node.finalCommentRef;
	if (node.parent)
		return route(
			state,
			node.parent,
			"architect",
			{ type: "child-reopened", child: key },
			envelope,
		);
	const tree = state.trees[key];
	if (!tree) {
		return [
			{
				kind: "controller",
				payload: {
					type: "triage",
					issue: key,
					preexistingChildren: node.children,
				},
			},
		];
	}
	if (tree.status === "lingering") {
		return route(state, key, "architect", { type: "reopened" }, envelope);
	}
	return [
		{ kind: "controller", payload: { type: "reactivation", issue: key } },
		{ kind: "probe", tree: key },
	];
}

function issueComment(
	state: LegionState,
	payload: JsonRecord,
	envelope: EnvelopeJson,
	config: ReducerConfig,
): Effect[] | undefined {
	const rawIssue = asRecord(payload.issue);
	const comment = asRecord(payload.comment);
	if (!rawIssue || !comment) return undefined;
	const repo = repository(payload);
	const number = numberValue(rawIssue.number);
	if (!repo || number === undefined) return [];
	indexComment(state, payload, repo);
	if (payload.action !== "created" || filtered(comment, config)) return [];
	const author = stringValue(asRecord(comment.user)?.login) ?? "";
	const body = stringValue(comment.body) ?? "";
	const url = stringValue(comment.html_url) ?? "";
	const dispatch = state.dispatchThreads.find(
		(thread) => thread.repo === repo && thread.thread === number,
	);
	if (dispatch) {
		const dispatchRole = LEGION_ROLES.find((role) => role === dispatch.role);
		if (!dispatchRole) {
			throw new Error(`Invalid Legion dispatch role: ${dispatch.role}`);
		}
		return routeToken(
			state,
			dispatch.issue,
			roleToken(state.project, dispatch.issue, dispatchRole),
			{ type: "dispatch-reply", thread: number, author, body },
			envelope,
		);
	}
	if (rawIssue.pull_request !== undefined) {
		const pr = state.prs[`${repo}#${number}`];
		return pr
			? route(
					state,
					pr.key,
					"implementer",
					{ type: "pr-comment", author, body, url },
					envelope,
				)
			: [];
	}
	const key = keyFor(repo, number);
	return key
		? route(
				state,
				key,
				"architect",
				{ type: "issue-comment", author, body, url },
				envelope,
			)
		: [];
}

function reviewComment(
	state: LegionState,
	payload: JsonRecord,
	envelope: EnvelopeJson,
	config: ReducerConfig,
): Effect[] | undefined {
	const pullRequest = asRecord(payload.pull_request);
	const comment = asRecord(payload.comment);
	if (!pullRequest || !comment || asRecord(payload.issue)) return undefined;
	const repo = repository(payload);
	const number = numberValue(pullRequest.number);
	if (
		!repo ||
		number === undefined ||
		payload.action !== "created" ||
		filtered(comment, config)
	)
		return [];
	const pr = state.prs[`${repo}#${number}`];
	if (!pr) return [];
	return route(
		state,
		pr.key,
		"implementer",
		{
			type: "pr-review-comment",
			author: stringValue(asRecord(comment.user)?.login) ?? "",
			body: stringValue(comment.body) ?? "",
			path: stringValue(comment.path) ?? "",
			url: stringValue(comment.html_url) ?? "",
		},
		envelope,
	);
}

function review(
	state: LegionState,
	payload: JsonRecord,
	envelope: EnvelopeJson,
): Effect[] | undefined {
	const pullRequest = asRecord(payload.pull_request);
	const rawReview = asRecord(payload.review);
	if (!pullRequest || !rawReview) return undefined;
	const repo = repository(payload);
	const number = numberValue(pullRequest.number);
	if (!repo || number === undefined || payload.action !== "submitted")
		return [];
	const pr = state.prs[`${repo}#${number}`];
	if (!pr) return [];
	const decision = (stringValue(rawReview.state) ?? "").toLowerCase();
	const prior = pr.reviewDecision;
	if (decision === "approved" || decision === "changes_requested")
		pr.reviewDecision = decision;
	const result = route(
		state,
		pr.key,
		"implementer",
		{
			type: "pr-review",
			state: decision,
			author: stringValue(asRecord(rawReview.user)?.login) ?? "",
			body: stringValue(rawReview.body) ?? "",
		},
		envelope,
	);
	result.push({ kind: "approval-status", repo, pr: number, sha: pr.headSha });
	if (decision === "approved" && prior !== "approved" && isGreen(pr)) {
		result.push(
			...route(
				state,
				pr.key,
				"architect",
				{ type: "pr-ready", pr: number },
				envelope,
			),
		);
	}
	return result;
}

function pullRequest(
	state: LegionState,
	payload: JsonRecord,
	envelope: EnvelopeJson,
): Effect[] | undefined {
	const raw = asRecord(payload.pull_request);
	if (!raw || payload.review !== undefined || payload.comment !== undefined)
		return undefined;
	const repo = repository(payload);
	const number = numberValue(raw.number);
	if (!repo || number === undefined) return [];
	const prKey = `${repo}#${number}`;
	const head = asRecord(raw.head);
	const branch = stringValue(head?.ref);
	const sha = stringValue(head?.sha);

	if (payload.action === "opened") {
		const key = branch ? issueForBranch(repo, branch) : undefined;
		if (!key || !state.issues[key] || !sha) return [];
		state.prs[prKey] = {
			key,
			repo: repo as `${string}/${string}`,
			number,
			headSha: sha,
			checks: {},
			firstRedEmitted: false,
			settledRedEmitted: false,
			greenEmitted: false,
			lastEventAt: envelope.issued_at,
			fixAttempts: 0,
		};
		state.prByBranch[`${repo}@${branch}`] = prKey;
		return route(
			state,
			key,
			"implementer",
			{ type: "pr-opened", pr: number, url: stringValue(raw.html_url) ?? "" },
			envelope,
		);
	}

	const pr = state.prs[prKey];
	if (!pr) return [];
	if (payload.action === "synchronize") {
		if (!sha) return [];
		if (wasRed(pr)) pr.fixAttempts += 1;
		pr.headSha = sha;
		pr.checks = {};
		pr.firstRedEmitted = false;
		pr.settledRedEmitted = false;
		pr.greenEmitted = false;
		delete pr.reviewDecision;
		return [{ kind: "approval-status", repo, pr: number, sha }];
	}
	if (payload.action === "closed" && raw.merged === false) {
		delete state.prs[prKey];
		removeBranchMappings(state, prKey);
		return route(
			state,
			pr.key,
			"architect",
			{ type: "pr-closed-unmerged", pr: number },
			envelope,
		);
	}
	return [];
}

export function reduceGithubEvent(
	state: LegionState,
	topic: string,
	envelope: EnvelopeJson,
	config: ReducerConfig,
): Effect[] {
	if (
		/^notifications\.github\.[^.]+\.[^.]+\.pr\.\d+\.check(?:\.|$)/.test(topic)
	)
		return [];
	const payload = payloadFrom(envelope);
	if (!payload) return [];
	const repo = repository(payload);
	if (
		repo &&
		stringValue(payload.ref)?.startsWith("refs/heads/legion/issue-")
	) {
		indexPush(state, payload, repo);
		return [];
	}
	return (
		ingress(state, payload, config) ??
		subIssue(state, payload, envelope) ??
		issueComment(state, payload, envelope, config) ??
		reviewComment(state, payload, envelope, config) ??
		review(state, payload, envelope) ??
		pullRequest(state, payload, envelope) ??
		issueEvent(state, payload, envelope) ??
		[]
	);
}

export function reduceCiEmission(
	state: LegionState,
	repo: string,
	number: number,
	emission: CiEmission,
	config: ReducerConfig,
): Effect[] {
	const pr = state.prs[`${repo}#${number}`];
	if (!pr || pr.headSha !== emission.sha) return [];
	const envelope = {
		event_id: `ci:${repo}#${number}:${emission.sha}`,
		issued_at: pr.lastEventAt,
	};
	if (emission.type === "ci-green") {
		return pr.reviewDecision === "approved"
			? route(
					state,
					pr.key,
					"architect",
					{ type: "pr-ready", pr: number },
					envelope,
				)
			: [];
	}
	return pr.fixAttempts >= config.maxFixAttempts
		? route(
				state,
				pr.key,
				"architect",
				{ type: "pr-blocked", pr: number, attempts: pr.fixAttempts },
				envelope,
			)
		: [];
}
