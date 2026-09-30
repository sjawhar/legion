/**
 * `WRITES`: every write the evaluation proxy answers itself, recorded and never sent. Each
 * handler follows its Go server handler (`packages/envoy/internal/dispatch/api`): the same
 * validation, the same answer, and the same events on the same log, so later reads show the
 * write as the server would. What it cannot follow — a server-side judgement such as the
 * duplicate check, a session's liveness, a delivery, a document edit — it answers `501`.
 */
import { randomUUID } from "node:crypto";
import {
  type Artifact,
  type ArtifactApproval,
  type ArtifactUploadResponse,
  ASK_URGENCIES,
  type Ask,
  type AskResolution,
  type Comment,
  ISSUE_STATUSES,
  type Issue,
  type IssueComponents,
  type IssueDetails,
  type IssueSummary,
  type Message,
  type MessageEventPayload,
  type MessageRead,
} from "@legion/contracts";

import { isRecord } from "./exclude";
import {
  type Answer,
  api,
  append,
  type CommentPayload,
  canonicalText,
  closedIssue,
  created,
  currentArtifact,
  currentAsk,
  currentComment,
  currentIssue,
  type EventOwner,
  enc,
  follow,
  notFound,
  now,
  ok,
  projectOf,
  Refusal,
  refused,
  requireOpenIssue,
  requireProjectDocument,
  type StoredAsk,
  storeDocument,
  threadTarget,
  unmodelledWrite,
  version,
  type WriteContext,
  type WriteRoute,
  withApproval,
  withLog,
} from "./overlay";

/** Keys the proxy gives the issues it creates: past any real issue number. */
const CREATED_ISSUE_NUMBER_BASE = 900_000;
const PROJECT_KEY = /^[A-Z][A-Z0-9]*$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** The spec a new issue gets when its create names none: the server's
 *  `defaultIssueSpecMarkdown` as its canonical serializer stores it, emphasis as `*`. */
const DEFAULT_SPEC = `## Summary

*Three sentences at most, in plain words: the problem, what changes for whom, and how we will know it worked.*

## New since we talked

*One plain sentence per design point the human did not settle in conversation, marked inferred with the reasoning.*

## Acceptance

*List numbered outcomes that name what a user will observe and the check that proves each one.*

## Requirements

*What must hold, and where each came from: a quoted human sentence, or inferred plus the reasoning.*

## Design

*The files, components, routes, and data flow that change.*

## Errors

*Use a condition | behaviour table; do not specify silent fallbacks.*

## Testing

*Map every acceptance line to the proof that exercises it.*

## Rejected

*List each considered alternative and the reason it was rejected.*
`;

const APPROVAL_OPTIONS = [
  { label: "Approve", description: "Approve this version of the document." },
  { label: "Request changes", description: "Say what must change before it can be approved." },
];

const trimmed = (value: unknown) => (typeof value === "string" ? value.trim() : "");
const invalid = (code: string, message: string) => new Refusal(refused(400, code, message));

/** The server's `capExceededError`; lengths are UTF-16 units, as JavaScript counts them. */
function capped(field: string, text: string, limit: number): void {
  if (text.length > limit) {
    throw invalid(
      "CAP_EXCEEDED",
      `${field} is ${text.length - limit} characters over the ${limit}-character limit (${text.length}/${limit})`
    );
  }
}

/** The server's `countExceededError`. */
function counted(code: string, field: string, count: number, limit: number): void {
  if (count > limit) {
    throw invalid(
      code,
      `${field} is ${count - limit} over the ${limit}-item limit (${count}/${limit})`
    );
  }
}

/** The server's `normalizeIssueLabels`. */
function labelsOf(value: unknown): string[] {
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value)) throw invalid("INVALID_JSON", "invalid JSON body");
  counted("LABELS_INPUT", "labels", value.length, 20);
  const labels: string[] = [];
  const seen = new Set<string>();
  for (const raw of value) {
    const label = trimmed(raw);
    if (label === "" || label.length > 40) {
      throw invalid("LABELS_INPUT", "each label must be 1 to 40 characters");
    }
    if (seen.has(label.toLowerCase())) continue;
    seen.add(label.toLowerCase());
    labels.push(label);
  }
  return labels;
}

/** The server's `parseIssuePriority`: undefined when absent. */
function priorityOf(input: Readonly<Record<string, unknown>>): Issue["priority"] | undefined {
  if (!Object.hasOwn(input, "priority")) return undefined;
  const value = input.priority;
  if (value === null || value === 0 || value === 1 || value === 2 || value === 3) return value;
  throw invalid("INVALID_PRIORITY", "priority must be an integer from 0 to 3 or null");
}

/** The server's `validateAskQuestion` and `validateAskOptions`. */
function askText(question: unknown, options: unknown): Pick<Ask, "question" | "options"> {
  const text = typeof question === "string" ? question : "";
  if (text.trim() === "") throw invalid("INVALID_ASK", "ask question is required");
  capped("question", text, 800);
  const list = options ?? [];
  if (!Array.isArray(list)) throw invalid("INVALID_JSON", "invalid JSON body");
  counted("CAP_EXCEEDED", "options", list.length, 8);
  const labels = new Set<string>();
  return {
    question: text,
    options: list.map((option: unknown) => {
      const label = trimmed(isRecord(option) ? option.label : undefined);
      if (label === "") throw invalid("INVALID_ASK", "ask option labels are required");
      if (labels.has(label)) throw invalid("INVALID_ASK", "ask option labels must be unique");
      labels.add(label);
      const description = isRecord(option) ? option.description : undefined;
      return typeof description === "string" && description !== ""
        ? { label, description }
        : { label };
    }),
  };
}

function urgencyOf(value: unknown): Ask["urgency"] {
  const urgency = trimmed(value) || "med";
  const known = ASK_URGENCIES.find((each) => each === urgency);
  if (!known) throw invalid("INVALID_ASK", "ask urgency must be low, med, high, or blocking");
  return known;
}

/** The server's `rank.after`: a key that sorts strictly after `previous`. */
function rankAfter(previous: string): string {
  const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
  for (let index = 0; index < previous.length; index += 1) {
    const digit = alphabet.indexOf(previous[index] ?? "");
    if (digit < alphabet.length - 1) return previous.slice(0, index) + alphabet[digit + 1];
  }
  return `${previous}U`;
}

/** The server's `artifactSlug`: letters and digits kept, every other run a single dash. */
function slugOf(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^\p{L}\p{N}]+/gu, "-")
    .replace(/^-+|-+$/g, "");
  return slug === "" ? "artifact" : slug;
}

/** Where an ask or comment lives: an issue, or a project document. */
type Owner = { readonly issue: string } | { readonly document: Artifact };

const ownerIssue = (owner: Owner) => ("issue" in owner ? owner.issue : null);
const ownerDocument = (owner: Owner) => ("issue" in owner ? null : owner.document.id);
const onOwner = (owner: Owner, row: { issue_key: string | null; artifact_id?: string | null }) =>
  row.issue_key === ownerIssue(owner) && (row.artifact_id ?? null) === ownerDocument(owner);

/** The owner of an ask, comment or document write: an open issue, or a project document. */
async function writeOwner(context: WriteContext): Promise<Owner> {
  if (context.params.key !== undefined) {
    await requireOpenIssue(context, context.params.key);
    return { issue: context.params.key };
  }
  const document = await currentArtifact(context, context.params.artifact ?? "");
  if (!document) throw new Refusal(notFound());
  requireProjectDocument(document);
  return { document };
}

/** The owner an existing ask or comment answers to, refused when it is closed or gone. */
async function ownerOf(
  context: WriteContext,
  row: { issue_key: string | null; artifact_id?: string | null }
): Promise<Owner> {
  if (row.issue_key !== null) {
    await requireOpenIssue(context, row.issue_key);
    return { issue: row.issue_key };
  }
  const document = await currentArtifact(context, row.artifact_id ?? "");
  if (!document) throw new Refusal(refused(404, "ARTIFACT_NOT_FOUND", "artifact not found"));
  return { document };
}

/** An ask as its `ask.*` event payload carries it: with the backlink count a read attaches. */
function askPayload(context: WriteContext, ask: StoredAsk): StoredAsk {
  const before = context.overlay.upstreamAsks.get(ask.id);
  return { ...ask, referenced_by_count: before?.referenced_by_count ?? 0 };
}

/** The comment event payload the server writes (`commentEventPayload`). */
function commentPayload(
  comment: Comment,
  owner: Owner,
  thread: { ask?: Ask; turn?: string; root?: string },
  created: boolean
): CommentPayload {
  return {
    ...comment,
    artifact_name: "",
    ...("document" in owner
      ? { project_key: owner.document.project, artifact_slug: owner.document.slug }
      : {}),
    ...(thread.ask ? { ask_question: thread.ask.question, ask_state: thread.ask.state } : {}),
    ...(thread.turn === "human" || thread.turn === "agent" ? { ask_waiting_on: thread.turn } : {}),
    ...(thread.root ? { thread_root_id: thread.root } : {}),
    suppress_route: false,
    suppressed_authors: created ? [] : null,
  };
}

async function createIssue(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  if (trimmed(input.external) !== "") {
    return unmodelledWrite("does not model an issue created from an external reference");
  }
  if (input.force !== true) {
    return unmodelledWrite(
      "cannot run the server's duplicate check, so it records an issue create only with force: true"
    );
  }
  if (input.assignee !== undefined) {
    return unmodelledWrite("cannot check an assignee against the sign-in allowlist");
  }
  if (input.components !== undefined)
    return unmodelledWrite("does not model component attachments");
  const project = trimmed(input.project);
  const title = trimmed(input.title);
  if (!PROJECT_KEY.test(project) || title === "") {
    throw invalid("INVALID_ISSUE", "project and title are required");
  }
  const labels = labelsOf(input.labels);
  const priority = priorityOf(input) ?? null;
  const parentKey = trimmed(input.parent);
  const parent = parentKey === "" ? undefined : await currentIssue(context, parentKey);
  if (parentKey !== "" && !parent)
    throw invalid("PARENT_INPUT", `parent issue ${parentKey} not found`);
  if (parent && parent.project !== project) {
    throw invalid("PARENT_INPUT", "parent must be in the same project");
  }
  const projects = await context.upstream.json<{ key: string }[]>(api("/projects"));
  if (!projects?.some((each) => each.key === project)) return notFound();
  const listed =
    (await context.upstream.json<IssueSummary[]>(api("/issues"), `?project=${enc(project)}`)) ?? [];
  const ranks = [...listed, ...overlay.issues.values()]
    .filter((issue) => projectOf(issue.key) === project)
    .map((issue) => issue.rank)
    .sort();
  const last = ranks.at(-1);
  overlay.issueCount += 1;
  const number = CREATED_ISSUE_NUMBER_BASE + overlay.issueCount;
  const key = `${project}-${number}`;
  overlay.created.add(key);
  const spec =
    typeof input.spec === "string" && input.spec.trim() !== "" ? input.spec : DEFAULT_SPEC;
  const document = storeDocument(
    overlay,
    { issue: key, project },
    "spec.md",
    "spec",
    spec,
    actor,
    true
  );
  // A child inherits the components its nearest attached ancestor chose.
  const inherited: IssueComponents | undefined =
    parent && (parent.components.mode !== "inherit" || parent.components.inherited_from !== null)
      ? { ...parent.components, inherited_from: parent.components.inherited_from ?? parent.key }
      : undefined;
  const at = now();
  const issue: Issue = {
    key,
    project,
    number,
    title,
    status: "triage",
    priority,
    rank: last === undefined ? "U" : rankAfter(last),
    labels,
    parent: parent?.key ?? null,
    assignee:
      actor.kind === "session" && actor.owner
        ? actor.owner.toLowerCase()
        : (parent?.assignee ?? null),
    claim: null,
    components: inherited ?? {
      mode: "inherit",
      ids: [],
      unknown: [],
      reason: null,
      inherited_from: null,
    },
    external_links: [],
    route: null,
    created_by: actor,
    created_at: at,
    updated_at: at,
    closed_at: null,
    primary_artifact_id: document.artifact.id,
    last_seq: 0,
  };
  overlay.issues.set(key, issue);
  await append(context, { issue: key }, actor, ({ seq }) => ({
    type: "issue.created",
    payload: { ...issue, last_seq: seq },
  }));
  return created(withLog(overlay, issue));
}

async function updateIssue(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  const key = context.params.key ?? "";
  for (const field of ["rank", "route", "external_links", "assignee", "parent", "components"]) {
    if (Object.hasOwn(input, field)) return unmodelledWrite(`does not model an issue's ${field}`);
  }
  const title = input.title === undefined ? undefined : trimmed(input.title);
  if (title === "") throw invalid("INVALID_ISSUE", "title must not be blank");
  const status = input.status === undefined ? undefined : trimmed(input.status);
  if (status === "") throw invalid("INVALID_ISSUE", "status must not be blank");
  if (status !== undefined && !ISSUE_STATUSES.some((known) => known === status)) {
    throw invalid("INVALID_STATUS", "status is not in the Legion lifecycle");
  }
  const labels = input.labels === undefined ? undefined : labelsOf(input.labels);
  const priority = priorityOf(input);
  const before = await currentIssue(context, key);
  if (!before) return notFound();
  const changes = title !== undefined || labels !== undefined || priority !== undefined;
  // A closed issue takes only a reopening status.
  if (before.closed_at !== null && (changes || status === undefined || status === "done")) {
    throw closedIssue();
  }
  if (!changes && status === undefined) return ok(before);
  const at = now();
  const closing = status === "done";
  const after: Issue = {
    ...before,
    ...(title === undefined ? {} : { title }),
    ...(status === undefined ? {} : { status, closed_at: closing ? at : null }),
    ...(labels === undefined ? {} : { labels }),
    ...(priority === undefined ? {} : { priority }),
    // Closing releases the claim.
    ...(closing ? { claim: null } : {}),
    updated_at: at,
  };
  overlay.issues.set(key, after);
  const closed = status !== undefined && before.status !== after.status && closing;
  await append(context, { issue: key }, actor, ({ seq }) => ({
    type: closed ? "issue.closed" : "issue.updated",
    payload: { ...after, last_seq: seq },
  }));
  if (closing && before.claim) {
    const previous = before.claim;
    await append(context, { issue: key }, actor, () => ({
      type: "issue.released",
      payload: {
        key,
        status: after.status,
        claim: null,
        previous_claim: previous,
        reason: "closed",
      },
    }));
  }
  const parent = after.parent;
  if (status !== undefined && before.status !== after.status && parent !== null) {
    await append(context, { issue: parent }, actor, () => ({
      type: "child.status",
      payload: { child_key: key, from: before.status, to: after.status },
    }));
  }
  return ok(withLog(overlay, after));
}

async function claimIssue(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  const key = context.params.key ?? "";
  if (input.force === true) {
    throw new Refusal(
      refused(403, "HUMAN_ONLY", "only a human may force a claim away from a live session")
    );
  }
  const before = await currentIssue(context, key);
  if (!before) return notFound();
  if (before.closed_at !== null) throw closedIssue();
  const holder = before.claim?.actor;
  // A repeated claim keeps the time work started, and stays out of the issue's log.
  if (holder?.kind === actor.kind && holder.id === actor.id) return ok(before);
  if (holder !== undefined) {
    return unmodelledWrite("cannot judge whether the session holding this claim is still live");
  }
  const at = now();
  const after: Issue = { ...before, claim: { actor, at }, updated_at: at };
  overlay.issues.set(key, after);
  await append(context, { issue: key }, actor, () => ({
    type: "issue.claimed",
    payload: { key, status: after.status, claim: after.claim, reason: "claimed" },
  }));
  return ok(withLog(overlay, after));
}

async function releaseIssue(context: WriteContext): Promise<Answer> {
  const { overlay, actor } = context;
  const key = context.params.key ?? "";
  const before = await currentIssue(context, key);
  if (!before) return notFound();
  const previous = before.claim;
  // Releasing nothing answers the issue, whether or not a close already cleared the claim.
  if (previous === null) return ok(before);
  if (previous.actor.kind !== actor.kind || previous.actor.id !== actor.id) {
    return unmodelledWrite("cannot judge whether the session holding this claim is still live");
  }
  const after: Issue = { ...before, claim: null, updated_at: now() };
  overlay.issues.set(key, after);
  await append(context, { issue: key }, actor, () => ({
    type: "issue.released",
    payload: {
      key,
      status: after.status,
      claim: null,
      previous_claim: previous,
      reason: "released",
    },
  }));
  return ok(withLog(overlay, after));
}

/** Records a new ask with its author following it and its `ask.opened` event. */
async function openAsk(context: WriteContext, owner: Owner, draft: StoredAsk): Promise<StoredAsk> {
  const { overlay, actor } = context;
  let ask = draft;
  await append(context, owner, actor, ({ id }) => {
    ask = { ...draft, opened_event_id: id };
    return { type: "ask.opened", payload: askPayload(context, ask) };
  });
  overlay.asks.set(ask.id, ask);
  overlay.created.add(ask.id);
  follow(overlay, ask.id, actor);
  return ask;
}

async function createAsk(context: WriteContext): Promise<Answer> {
  const { input, actor } = context;
  const kind = trimmed(input.kind);
  if (kind === "approval") throw invalid("ASK_KIND_INPUT", "approval asks are server-created only");
  if (kind === "action") {
    throw invalid(
      "ASK_KIND_INPUT",
      "the action ask kind was removed; open a question with the options you want, such as Done / Can't"
    );
  }
  if (kind !== "" && kind !== "question")
    throw invalid("ASK_KIND_INPUT", "ask kind must be question");
  const text = askText(input.question, input.options);
  const urgency = urgencyOf(input.urgency);
  if (input.anchor !== undefined && input.anchor !== null) {
    return unmodelledWrite("does not model an ask anchored in a document");
  }
  const owner = await writeOwner(context);
  return created(
    await openAsk(context, owner, {
      id: randomUUID(),
      issue_key: ownerIssue(owner),
      artifact_id: ownerDocument(owner),
      block_id: null,
      author: actor,
      kind: "question",
      ...text,
      multiple: input.multiple === true,
      urgency,
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 0,
      created_at: now(),
      edited_at: null,
    })
  );
}

/** Where a reply lands (`normalizeCommentThreadTarget`): under the ask its thread hangs from,
 *  or under the root comment of its thread. */
async function replyThread(
  context: WriteContext,
  owner: Owner
): Promise<{ ask?: StoredAsk; root?: Comment }> {
  const { input } = context;
  const replyTo = input.reply_to === undefined ? undefined : trimmed(input.reply_to);
  const askId = input.ask_id === undefined ? undefined : trimmed(input.ask_id);
  if (replyTo !== undefined && askId !== undefined) {
    throw invalid("INVALID_COMMENT", "reply_to and ask_id cannot both be set");
  }
  if (replyTo !== undefined) {
    if (!UUID.test(replyTo)) throw invalid("INVALID_COMMENT", "reply_to must be a full comment id");
    const elsewhere = invalid("INVALID_COMMENT", "reply_to must identify a comment on this owner");
    let current = await currentComment(context, replyTo);
    for (let depth = 0; current?.ask_id === null && current.reply_to !== null; depth += 1) {
      if (!onOwner(owner, current) || depth > 64) throw elsewhere;
      current = await currentComment(context, current.reply_to);
    }
    if (!current || !onOwner(owner, current)) throw elsewhere;
    if (current.ask_id === null) return { root: current };
    return { ask: await currentAsk(context, current.ask_id) };
  }
  if (askId === undefined) return {};
  if (!UUID.test(askId)) {
    throw new Refusal(
      unmodelledWrite("does not list the owner's open asks for an ask id that is not a UUID")
    );
  }
  const ask = await currentAsk(context, askId);
  if (!ask || !onOwner(owner, ask)) {
    throw invalid("INVALID_COMMENT", "ask_id must identify an ask on this owner");
  }
  return { ask };
}

async function createComment(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  if (input.suggestion !== undefined && input.suggestion !== null) {
    return unmodelledWrite("does not model a suggestion");
  }
  const body = typeof input.body === "string" ? input.body : "";
  if (body.trim() === "") throw invalid("INVALID_COMMENT", "comment body is required");
  capped("body", body, 2000);
  const mentions = Array.isArray(input.mentions) ? input.mentions : [];
  if (mentions.length === 0 && input.delivery !== undefined) {
    throw invalid("MENTION_INPUT", "delivery requires mentions");
  }
  if (mentions.length > 0) return unmodelledWrite("does not deliver mentions");
  if (input.anchor !== undefined && input.anchor !== null) {
    return unmodelledWrite("does not model a comment anchored in a document");
  }
  const owner = await writeOwner(context);
  const { ask, root } = await replyThread(context, owner);
  if (input.turn !== undefined) {
    if (!ask) throw invalid("TURN_REQUIRES_ASK", "turn is only valid on a reply to an ask");
    if (input.turn !== "human" && input.turn !== "agent") {
      throw invalid("INVALID_COMMENT", "turn must be human or agent");
    }
  }
  // Only an open ask has a turn to hold; a session's reply hands it to the human unless it is a
  // progress note.
  const turn = ask?.state === "open" ? (input.turn === "agent" ? "agent" : "human") : null;
  if (root?.resolved) {
    const reopened: Comment = { ...root, resolved: false, resolved_by: null, resolved_at: null };
    overlay.comments.set(reopened.id, reopened);
    await append(context, owner, actor, () => ({
      type: "comment.reopened",
      payload: commentPayload(reopened, owner, {}, false),
    }));
  }
  const comment: Comment = {
    id: randomUUID(),
    issue_key: ownerIssue(owner),
    artifact_id: ownerDocument(owner),
    author: actor,
    body,
    anchor: null,
    reply_to: root?.id ?? null,
    ask_id: ask?.id ?? null,
    turn,
    resolved: false,
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    suggestion: null,
    created_at: now(),
    mentions: [],
    deliveries: [],
  };
  overlay.comments.set(comment.id, comment);
  overlay.created.add(comment.id);
  if (ask) follow(overlay, ask.id, actor);
  await append(context, owner, actor, () => ({
    type: "comment.created",
    payload: commentPayload(comment, owner, { ask, turn: turn ?? undefined, root: root?.id }, true),
  }));
  return created(comment);
}

/** What a reply's event says about the thread it joins (`messageReplyThread`). */
function threadOf(
  parent: Message,
  rootTarget: string
): Pick<MessageEventPayload, "reply_body" | "thread_target"> {
  return {
    reply_body: [...parent.body].slice(0, 160).join(""),
    ...(rootTarget === "" ? {} : { thread_target: rootTarget }),
  };
}

async function createMessage(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  const key = context.params.key ?? "";
  const body = typeof input.body === "string" ? input.body : "";
  if (body.trim() === "") throw invalid("INVALID_MESSAGE", "message body is required");
  capped("body", body, 2000);
  if (input.target !== undefined || input.delivery !== undefined) {
    if (trimmed(input.target) === "") throw invalid("MESSAGE_INPUT", "delivery requires target");
    return unmodelledWrite("does not deliver a targeted message");
  }
  if (input.urgency !== undefined && !ASK_URGENCIES.some((known) => known === input.urgency)) {
    throw invalid("MESSAGE_INPUT", "urgency must be one of low, med, high, blocking");
  }
  await requireOpenIssue(context, key);
  const inReplyTo = input.in_reply_to === undefined ? null : trimmed(input.in_reply_to);
  let parent: Message | undefined;
  if (inReplyTo !== null) {
    const elsewhere = invalid("MESSAGE_INPUT", "in_reply_to must identify a message on this issue");
    if (!UUID.test(inReplyTo)) throw elsewhere;
    parent =
      overlay.messages.get(inReplyTo) ??
      (
        await context.upstream.json<MessageRead>(
          api(`/issues/${enc(key)}/messages/${enc(inReplyTo)}`)
        )
      )?.message;
    if (parent?.issue_key !== key) throw elsewhere;
  }
  const message: Message = {
    id: randomUUID(),
    issue_key: key,
    author: actor,
    body,
    target: null,
    in_reply_to: inReplyTo,
    created_at: now(),
    deliveries: [],
  };
  const thread = parent ? threadOf(parent, await threadTarget(context, parent)) : {};
  overlay.messages.set(message.id, message);
  overlay.created.add(message.id);
  await append(context, { issue: key }, actor, () => ({
    type: parent ? "message.answered" : "message.created",
    payload: { ...message, ...thread },
  }));
  return created(message);
}

async function replyToDelivery(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor, query } = context;
  const attempt = typeof input.attempt === "number" ? input.attempt : 0;
  const body = typeof input.body === "string" ? input.body : undefined;
  if (attempt < 1 || (body === undefined) === (typeof input.error !== "string")) {
    throw new Refusal(
      refused(403, "REPLY_FORBIDDEN", "reply requires one body or error for a delivery attempt")
    );
  }
  if (body?.trim() === "") throw invalid("MESSAGE_INPUT", "message body is required");
  const followUp = query.get("follow_up");
  if (followUp !== null && followUp !== "true" && followUp !== "false") {
    throw invalid("MESSAGE_INPUT", "follow_up must be true or false");
  }
  const id = context.params.message ?? "";
  // A message the agent wrote was delivered to nobody.
  if (overlay.created.has(id)) {
    throw new Refusal(refused(404, "MESSAGE_NOT_FOUND", "message delivery not found"));
  }
  const thread = await context.upstream.json<MessageRead>(
    api(`/messages/${enc(id)}`),
    `?session=${enc(actor.id)}`
  );
  const message = thread && [thread.message, ...thread.replies].find((each) => each.id === id);
  if (!thread || !message)
    throw new Refusal(refused(404, "MESSAGE_NOT_FOUND", "message not found"));
  if (message.issue_key !== null) await requireOpenIssue(context, message.issue_key);
  const delivery = message.deliveries.find((each) => each.attempt === attempt);
  if (!delivery) throw new Refusal(refused(404, "MESSAGE_NOT_FOUND", "message delivery not found"));
  if (delivery.session_id !== actor.id) {
    throw new Refusal(
      refused(403, "REPLY_FORBIDDEN", "session may reply only to its own delivery")
    );
  }
  const answeredWith = overlay.answered.get(id)?.get(attempt) ?? delivery.reply_id;
  if (answeredWith !== null) {
    // Without a follow-up, every call on an answered attempt is a retry of its answer.
    if (body === undefined || followUp !== "true") {
      const answer =
        overlay.messages.get(answeredWith) ??
        thread.replies.find((each) => each.id === answeredWith);
      return answer ? ok({ ...answer, duplicate: true }) : notFound();
    }
    return unmodelledWrite("does not model a follow-up to an answered delivery");
  }
  if (body === undefined) return unmodelledWrite("does not record a delivery failure");
  const reply: Message = {
    id: randomUUID(),
    issue_key: message.issue_key,
    author: actor,
    body,
    target: message.target,
    in_reply_to: message.id,
    created_at: now(),
    deliveries: [],
  };
  overlay.messages.set(reply.id, reply);
  overlay.created.add(reply.id);
  overlay.answered.set(id, new Map([...(overlay.answered.get(id) ?? []), [attempt, reply.id]]));
  const issueKey = reply.issue_key;
  if (issueKey !== null) {
    await append(context, { issue: issueKey }, actor, () => ({
      type: "message.answered",
      payload: { ...reply, ...threadOf(message, thread.message.target ?? "") },
    }));
  }
  return created(reply);
}

/** The documents an upload's name and slug are unique among (`coalesce(issue_key, project_key)`). */
async function siblingDocuments(
  context: WriteContext,
  key: string | undefined,
  project: string
): Promise<Artifact[]> {
  const { overlay, upstream } = context;
  if (key !== undefined) {
    const read = overlay.created.has(key)
      ? undefined
      : await upstream.json<IssueDetails>(api(`/issues/${enc(key)}`));
    return [...(read?.artifacts ?? []), ...overlay.documentsOf((each) => each.issue_key === key)];
  }
  const listed = await upstream.json<Artifact[]>(
    api(`/projects/${enc(project)}/artifacts`),
    "?unlinked=true"
  );
  if (!listed) throw new Refusal(refused(404, "PROJECT_NOT_FOUND", "project not found"));
  return [
    ...listed,
    ...overlay.documentsOf((each) => each.project === project && each.issue_key === null),
  ];
}

async function uploadDocument(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  const fields = ["name", "content", "primary", "summary", "actor", "file"];
  if (Object.keys(input).some((field) => !fields.includes(field))) {
    throw invalid("INVALID_JSON", "invalid JSON body");
  }
  if (input.primary !== undefined)
    throw invalid("ARTIFACT_INPUT", "primary is fixed at issue creation");
  if (input.file !== undefined) return unmodelledWrite("does not model a file upload");
  const name = trimmed(input.name);
  if (name === "") throw invalid("INVALID_ARTIFACT", "artifact name is required");
  const content = typeof input.content === "string" ? input.content : "";
  if (content.trim() === "") throw invalid("ARTIFACT_INPUT", "inline artifact content is required");
  const summary = trimmed(input.summary);
  const key = context.params.key;
  const project =
    key === undefined
      ? (context.params.project ?? "")
      : (await requireOpenIssue(context, key)).project;
  const siblings = await siblingDocuments(context, key, project);
  const named = siblings.find((artifact) => artifact.name === name);
  if (named) {
    // Uploading a name the owner already has saves a new version of that document.
    const stored = overlay.documents.get(named.id);
    if (!stored) return unmodelledWrite("does not replace a document the server holds");
    const next = version((named.versions.at(-1)?.number ?? 0) + 1, actor, summary);
    const answer: ArtifactUploadResponse = { artifact: stored.artifact, version: next };
    stored.artifact = { ...stored.artifact, versions: [...stored.artifact.versions, next] };
    stored.markdown = canonicalText(content);
    const owner: EventOwner = key !== undefined ? { issue: key } : { document: stored.artifact };
    await append(context, owner, actor, () => ({
      type: "artifact.version",
      payload: { artifact_id: stored.artifact.id, name: stored.artifact.name, version: next },
    }));
    return created(answer);
  }
  const taken = new Set(siblings.map((artifact) => artifact.slug));
  const base = slugOf(name);
  let slug = base;
  for (let suffix = 2; taken.has(slug); suffix += 1) slug = `${base}-${suffix}`;
  const stored = storeDocument(
    overlay,
    { issue: key ?? null, project },
    name,
    slug,
    content,
    actor,
    false
  );
  const [first] = stored.artifact.versions;
  if (!first) throw new Error("a stored document has its first version");
  const initial = summary === "" ? first : { ...first, named: true, summary };
  stored.artifact = { ...stored.artifact, versions: [initial] };
  const owner: EventOwner = key !== undefined ? { issue: key } : { document: stored.artifact };
  await append(context, owner, actor, () => ({
    type: "artifact.created",
    payload: { artifact: stored.artifact },
  }));
  return created({ artifact: stored.artifact, version: initial } satisfies ArtifactUploadResponse);
}

/** The ask a transition changes, and its owner, refused as `transitionAskTx` refuses. */
async function askToChange(context: WriteContext): Promise<{ ask: StoredAsk; owner: Owner }> {
  const ask = await currentAsk(context, context.params.ask ?? "");
  if (!ask) throw new Refusal(notFound());
  if (ask.block_id) {
    throw new Refusal(unmodelledWrite("does not rewrite an ask that lives in a document block"));
  }
  return { ask, owner: await ownerOf(context, ask) };
}

async function resolveAsk(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  const kind: AskResolution["kind"] | undefined =
    input.kind === "retracted" || input.kind === "resolved" ? input.kind : undefined;
  const reason = trimmed(input.reason);
  if (kind === undefined || reason === "") {
    throw invalid("INVALID_RESOLUTION", "resolution requires a kind and reason");
  }
  const { ask, owner } = await askToChange(context);
  if (ask.state === "answered") {
    throw new Refusal(refused(409, "ASK_ANSWERED", "ask is already answered"));
  }
  if (ask.state === "resolved") {
    throw new Refusal(refused(409, "ASK_RESOLVED", "ask is already resolved"));
  }
  const document = ask.approval?.artifact_id ?? "";
  const approval = overlay.approvals.get(document);
  if (ask.kind === "approval" && approval?.now.ask_id !== ask.id) {
    return unmodelledWrite(
      "cannot derive a document's approval once the server's approval request is gone"
    );
  }
  const resolution: AskResolution = { kind, reason, actor, at: now() };
  const resolved: StoredAsk = { ...ask, state: "resolved", resolution };
  overlay.asks.set(ask.id, resolved);
  if (approval) overlay.approvals.set(document, { ...approval, now: approval.before });
  await append(context, owner, actor, () => ({
    type: "ask.resolved",
    payload: { ...askPayload(context, resolved), state: "resolved", resolution },
  }));
  return ok(resolved);
}

async function editAsk(context: WriteContext): Promise<Answer> {
  const { overlay, input, actor } = context;
  const given = (field: string) => input[field] !== undefined && input[field] !== null;
  if (!["question", "options", "multiple", "urgency"].some(given)) {
    throw invalid("INVALID_ASK", "ask edit requires at least one field");
  }
  // The row takes the text a document block would: line feeds alone.
  const lines = (text: unknown) => (typeof text === "string" ? text.replace(/\r\n?/g, "\n") : text);
  const options = Array.isArray(input.options)
    ? input.options.map((option: unknown) =>
        isRecord(option)
          ? { label: lines(option.label), description: lines(option.description) }
          : option
      )
    : input.options;
  const { ask, owner } = await askToChange(context);
  const text = askText(
    given("question") ? lines(input.question) : ask.question,
    given("options") ? options : ask.options
  );
  const urgency = given("urgency") ? urgencyOf(input.urgency) : ask.urgency;
  if (ask.state !== "open")
    throw new Refusal(refused(409, "ASK_NOT_OPEN", "only open asks may be edited"));
  if (ask.kind === "approval") {
    throw new Refusal(
      refused(
        409,
        "ASK_KIND_FIXED",
        "an approval ask's question and options are fixed; retract it and request approval again"
      )
    );
  }
  if (ask.author.kind !== actor.kind || ask.author.id !== actor.id) {
    throw new Refusal(refused(403, "NOT_AUTHOR", "only the asking session may edit an ask"));
  }
  const at = now();
  const previous = {
    question: ask.question,
    options: ask.options,
    multiple: ask.multiple,
    urgency: ask.urgency,
  };
  const edited: StoredAsk = {
    ...ask,
    ...text,
    multiple: given("multiple") ? input.multiple === true : ask.multiple,
    urgency,
    edited_at: at,
  };
  overlay.asks.set(ask.id, edited);
  overlay.askEdits.set(ask.id, [
    ...(overlay.askEdits.get(ask.id) ?? []),
    { previous, edited_by: actor, at },
  ]);
  await append(context, owner, actor, () => ({
    type: "ask.edited",
    payload: { ...askPayload(context, edited), previous, edited_by: actor },
  }));
  return ok(edited);
}

async function resolveComment(context: WriteContext): Promise<Answer> {
  const { overlay, actor } = context;
  const comment = await currentComment(context, context.params.comment ?? "");
  if (!comment) return notFound();
  const owner = await ownerOf(context, comment);
  const resolved: Comment = { ...comment, resolved: true, resolved_by: actor, resolved_at: now() };
  overlay.comments.set(resolved.id, resolved);
  await append(context, owner, actor, () => ({
    type: "comment.resolved",
    payload: commentPayload(resolved, owner, {}, false),
  }));
  return ok(resolved);
}

async function requestApproval(context: WriteContext): Promise<Answer> {
  const { overlay, actor } = context;
  const found = await currentArtifact(context, context.params.artifact ?? "");
  if (!found) return notFound();
  const artifact = withApproval(overlay, found);
  if (artifact.kind !== "doc") throw invalid("NOT_DOCUMENT", "only documents can be approved");
  const owner: Owner =
    artifact.issue_key !== null ? { issue: artifact.issue_key } : { document: artifact };
  if (artifact.issue_key !== null) await requireOpenIssue(context, artifact.issue_key);
  const latest = artifact.versions.at(-1)?.number ?? 0;
  if (latest === 0) {
    throw new Refusal(
      refused(409, "NO_VERSION", "the document has no settled version to approve yet")
    );
  }
  const approval: ArtifactApproval = artifact.approval ?? {
    state: "draft",
    latest_version: latest,
  };
  const answer = (ask: StoredAsk | null, version: number): Answer =>
    ok({ ask, artifact_id: artifact.id, version, approval });
  // An approval at the current version, or a request already open, is answered as it stands.
  if (approval.state === "approved") return answer(null, latest);
  const open = [...overlay.asks.values()].find(
    (ask) =>
      ask.kind === "approval" && ask.state === "open" && ask.approval?.artifact_id === artifact.id
  );
  if (open) return answer(open, open.approval?.version ?? latest);
  if (approval.state === "awaiting" && approval.ask_id) {
    const upstream = await currentAsk(context, approval.ask_id);
    if (upstream?.state === "open") return answer(upstream, upstream.approval?.version ?? latest);
  }
  const ask = await openAsk(context, owner, {
    id: randomUUID(),
    issue_key: ownerIssue(owner),
    artifact_id: ownerDocument(owner),
    block_id: null,
    author: actor,
    kind: "approval",
    question: `Approve ${artifact.name} (version ${latest})?`,
    options: APPROVAL_OPTIONS,
    multiple: false,
    urgency: "high",
    anchor: null,
    state: "open",
    answer: null,
    opened_event_id: 0,
    created_at: now(),
    edited_at: null,
    approval: { artifact_id: artifact.id, name: artifact.name, version: latest },
  });
  const awaiting: ArtifactApproval = {
    ...approval,
    state: "awaiting",
    requested_by: actor,
    ask_id: ask.id,
  };
  overlay.approvals.set(artifact.id, { now: awaiting, before: approval });
  return created({ ask, artifact_id: artifact.id, version: latest, approval: awaiting });
}

/** Every write the proxy answers itself. A write no entry matches is recorded and answered
 *  `501 EVAL_PROXY_UNMODELLED_WRITE`. */
export const WRITES = {
  "create-issue": { method: "POST", paths: ["issues"], handle: createIssue },
  "update-issue": { method: "PATCH", paths: ["issues/:key"], handle: updateIssue },
  claim: { method: "POST", paths: ["issues/:key/claim"], handle: claimIssue },
  release: { method: "DELETE", paths: ["issues/:key/claim"], handle: releaseIssue },
  ask: {
    method: "POST",
    paths: ["issues/:key/asks", "artifacts/:artifact/asks"],
    handle: createAsk,
  },
  comment: {
    method: "POST",
    paths: ["issues/:key/comments", "artifacts/:artifact/comments"],
    handle: createComment,
  },
  message: { method: "POST", paths: ["issues/:key/messages"], handle: createMessage },
  "message-reply": { method: "POST", paths: ["messages/:message/reply"], handle: replyToDelivery },
  document: {
    method: "POST",
    paths: ["issues/:key/artifacts", "projects/:project/artifacts"],
    handle: uploadDocument,
  },
  "resolve-ask": { method: "POST", paths: ["asks/:ask/resolve"], handle: resolveAsk },
  "edit-ask": { method: "PATCH", paths: ["asks/:ask"], handle: editAsk },
  "resolve-comment": {
    method: "POST",
    paths: ["comments/:comment/resolve"],
    handle: resolveComment,
  },
  "approval-request": {
    method: "POST",
    paths: ["artifacts/:artifact/approval-requests"],
    handle: requestApproval,
  },
  "document-edit": {
    method: "POST",
    paths: [
      "artifacts/:artifact/edits",
      "issues/:key/artifacts/:slug/edits",
      "projects/:project/artifacts/:slug/edits",
    ],
    handle: async () =>
      unmodelledWrite("does not apply document edits: it records the edit and changes no text"),
  },
} as const satisfies Record<string, WriteRoute>;

export type WriteName = keyof typeof WRITES;
