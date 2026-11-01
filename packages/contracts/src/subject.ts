import { utf8Bytes } from "./dispatch-text-rules";
import { sha256Hex } from "./sha256";

export const AGENT_TOPIC_PREFIX = "notifications.agent." as const;
export const ROLE_TOPIC_PREFIX = "notifications.role." as const;

export const LEGION_NOTICE_TOPIC_PREFIX = "notifications.legion." as const;

export type LegionNoticeSubject<
  Project extends string = string,
  Issue extends string = string,
> = `${typeof LEGION_NOTICE_TOPIC_PREFIX}${Project}.${Issue}`;

/** The Go daemon's controller topic for `project` (the project token): the notice family's one
 * member that names no issue, since no issue key can be `controller`. What the Go daemon publishes
 * here is listed at `notify.ControllerTopic` (packages/daemon/internal/notify). */
export function legionControllerNoticeSubject<Project extends string>(
  project: Project
): LegionNoticeSubject<Project, "controller"> {
  return `${LEGION_NOTICE_TOPIC_PREFIX}${project}.controller`;
}

export const DISPATCH_TOPIC_PREFIX = "notifications.dispatch." as const;
export const DISPATCH_ISSUE_TOPIC_PREFIX = `${DISPATCH_TOPIC_PREFIX}issue.` as const;
export const DISPATCH_DOCUMENT_TOPIC_PREFIX = `${DISPATCH_TOPIC_PREFIX}document.` as const;

export type DispatchIssueSubject<
  IssueKey extends string = string,
  Type extends string = string,
> = `${typeof DISPATCH_ISSUE_TOPIC_PREFIX}${IssueKey}.${Type}`;

export function dispatchIssueSubject<IssueKey extends string, Type extends string>(
  issueKey: IssueKey,
  type: Type
): DispatchIssueSubject<IssueKey, Type>;
export function dispatchIssueSubject(issueKey: string, type: string) {
  return `${DISPATCH_ISSUE_TOPIC_PREFIX}${issueKey}.${type}`;
}

export type DispatchDocumentSubject<
  Project extends string = string,
  Slug extends string = string,
  Type extends string = string,
> = `${typeof DISPATCH_DOCUMENT_TOPIC_PREFIX}${Project}.${Slug}.${Type}`;

export function dispatchDocumentSubject<
  Project extends string,
  Slug extends string,
  Type extends string,
>(project: Project, slug: Slug, type: Type): DispatchDocumentSubject<Project, Slug, Type>;
export function dispatchDocumentSubject(project: string, slug: string, type: string) {
  return `${DISPATCH_DOCUMENT_TOPIC_PREFIX}${project}.${slug}.${type}`;
}

export type AgentSubject<Session extends string = string> =
  `${typeof AGENT_TOPIC_PREFIX}${Session}`;

export function agentSubject<Session extends string>(session: Session): AgentSubject<Session>;
export function agentSubject(session: string) {
  return `${AGENT_TOPIC_PREFIX}${session}`;
}

export type GithubSubject<
  Owner extends string = string,
  Repo extends string = string,
  Kind extends string = string,
> = `notifications.github.${SanitizedSubjectSegment<Owner>}.${SanitizedSubjectSegment<Repo>}.${Kind}`;

/**
 * Every GitHub subject begins with its repository's owner and name, each one segment: a name may
 * hold a dot, which a NATS subject splits on, so both go through `sanitizeSubjectSegment`
 * (`sjawhar/.github` is `notifications.github.sjawhar._github`). The payload's `repo` is the exact
 * name; a subscriber matching the subject only is matching a dot and an underscore alike.
 */
function githubRepositoryPrefix(owner: string, repo: string): string {
  return `notifications.github.${sanitizeSubjectSegment(owner)}.${sanitizeSubjectSegment(repo)}`;
}

export function githubSubject<Owner extends string, Repo extends string, Kind extends string>(
  owner: Owner,
  repo: Repo,
  kind: Kind
): GithubSubject<Owner, Repo, Kind>;
export function githubSubject(owner: string, repo: string, kind: string) {
  return `${githubRepositoryPrefix(owner, repo)}.${kind}`;
}

export type SlackSubject<
  Team extends string = string,
  Channel extends string = string,
  Kind extends string = string,
> = `notifications.slack.${Team}.${Channel}.${Kind}`;

export function slackSubject<Team extends string, Channel extends string, Kind extends string>(
  team: Team,
  channel: Channel,
  kind: Kind
): SlackSubject<Team, Channel, Kind>;
export function slackSubject(team: string, channel: string, kind: string) {
  return `notifications.slack.${team}.${channel}.${kind}`;
}

/**
 * The characters a subject segment writes as `_`: a dot, which a NATS subject splits on, and
 * whitespace and the wildcards `*` and `>`, which a published subject may not hold. The one list
 * `sanitizeSubjectSegment`, its type and the Go side's `SanitizeSubjectSegment` (generated from it
 * by `scripts/gen-go.ts`) all read, so what the listener publishes and what a consumer expects
 * cannot drift apart.
 */
export const SUBJECT_SEGMENT_REPLACED = [".", " ", "\t", "\r", "\n", "*", ">"] as const;

type SubjectSegmentReplaced = (typeof SUBJECT_SEGMENT_REPLACED)[number];

/**
 * Makes a value one NATS subject segment, writing each of `SUBJECT_SEGMENT_REPLACED` as `_`; a
 * slash is kept. The Go coordinator's intake (`packages/daemon/internal/intake`) mirrors it for
 * the GitHub subjects' owner and name, where only a dot can occur.
 *
 * Note: this is intentionally lossy; subscribers needing the exact identifier
 * should inspect the envelope payload.
 */
export function sanitizeSubjectSegment(value: string): string {
  const replaced: readonly string[] = SUBJECT_SEGMENT_REPLACED;
  return Array.from(value, (char) => (replaced.includes(char) ? "_" : char)).join("");
}

// Tail-recursive through an accumulator, so the compiler takes a literal of any practical length
// rather than stopping at its instantiation depth after about 100 characters.
type SanitizedSubjectSegment<
  Value extends string,
  Done extends string = "",
> = Value extends `${infer Head}${infer Tail}`
  ? SanitizedSubjectSegment<Tail, `${Done}${Head extends SubjectSegmentReplaced ? "_" : Head}`>
  : `${Done}${Value}`;

export type SlackThreadSubject<
  Team extends string = string,
  Channel extends string = string,
  ThreadTs extends string = string,
  Kind extends string = string,
> = `notifications.slack.${Team}.${Channel}.thread.${SanitizedSubjectSegment<ThreadTs>}.${Kind}`;

export function slackThreadSubject<
  Team extends string,
  Channel extends string,
  ThreadTs extends string,
  Kind extends string,
>(
  team: Team,
  channel: Channel,
  threadTs: ThreadTs,
  kind: Kind
): SlackThreadSubject<Team, Channel, ThreadTs, Kind>;
export function slackThreadSubject(team: string, channel: string, threadTs: string, kind: string) {
  return `notifications.slack.${team}.${channel}.thread.${sanitizeSubjectSegment(threadTs)}.${kind}`;
}

export type GithubResourceSubject<
  Owner extends string = string,
  Repo extends string = string,
  ResourceType extends string = string,
  ResourceNumber extends string | number = string | number,
> = `notifications.github.${SanitizedSubjectSegment<Owner>}.${SanitizedSubjectSegment<Repo>}.${ResourceType}.${ResourceNumber}`;

export function githubResourceSubject<
  Owner extends string,
  Repo extends string,
  ResourceType extends string,
  ResourceNumber extends string | number,
>(
  owner: Owner,
  repo: Repo,
  resourceType: ResourceType,
  resourceNumber: ResourceNumber
): GithubResourceSubject<Owner, Repo, ResourceType, ResourceNumber>;
export function githubResourceSubject(
  owner: string,
  repo: string,
  resourceType: string,
  resourceNumber: number | string
) {
  return `${githubRepositoryPrefix(owner, repo)}.${resourceType}.${resourceNumber}`;
}

export type GithubPushRefType = "branch" | "tag";

/**
 * The longest subject the Envoy bus publishes on, in bytes: `maxSubjectBytes` in
 * `packages/envoy/internal/bus/nats.go`, the NATS server's 4 KiB max control line less the rest of
 * the longest protocol line nats.go sends. The bus refuses a longer subject before sending it, since
 * the server would close the connection over it. `scripts/gen-go.ts` emits it as
 * `contracts.SubjectMaxBytes`.
 */
export const SUBJECT_MAX_BYTES = 4032;

export type GithubPushSubject<
  Owner extends string = string,
  Repo extends string = string,
  RefType extends GithubPushRefType = GithubPushRefType,
  RefName extends string = string,
> = `notifications.github.${SanitizedSubjectSegment<Owner>}.${SanitizedSubjectSegment<Repo>}.push.${RefType}.${SanitizedSubjectSegment<RefName>}`;

/**
 * A push's subject: its ref name is one segment, sanitized. The ref is the one part of a GitHub
 * subject that GitHub bounds only as git does, which is not at all, so a subject past
 * `SUBJECT_MAX_BYTES` keeps of the segment what fits, cut between characters, followed by `~` and
 * the first 16 hex digits of the SHA-256 of the whole ref name's UTF-8, and is then at most
 * `SUBJECT_MAX_BYTES`: the bus publishes it, and two refs that share the kept text still have two
 * subjects. Git refuses `~` in a ref, so a cut segment is never another ref's whole one. The type
 * names the uncut subject; a subscriber that needs the exact ref reads the payload's `ref`. The Go
 * side's `GithubPushSubject` (generated by `scripts/gen-go.ts`) cuts the same way.
 */
export function githubPushSubject<
  Owner extends string,
  Repo extends string,
  RefType extends GithubPushRefType,
  RefName extends string,
>(
  owner: Owner,
  repo: Repo,
  refType: RefType,
  refName: RefName
): GithubPushSubject<Owner, Repo, RefType, RefName>;
export function githubPushSubject(
  owner: string,
  repo: string,
  refType: GithubPushRefType,
  refName: string
) {
  const prefix = `${githubRepositoryPrefix(owner, repo)}.push.${refType}.`;
  const segment = sanitizeSubjectSegment(refName);
  if (utf8Bytes(prefix) + utf8Bytes(segment) <= SUBJECT_MAX_BYTES) {
    return prefix + segment;
  }
  const suffix = `~${sha256Hex(new TextEncoder().encode(refName)).slice(0, 16)}`;
  let room = SUBJECT_MAX_BYTES - utf8Bytes(prefix) - suffix.length;
  let kept = "";
  for (const char of segment) {
    const size = utf8Bytes(char);
    if (size > room) {
      break;
    }
    kept += char;
    room -= size;
  }
  return prefix + kept + suffix;
}

export type GithubWorkflowAction = "requested" | "in_progress" | "completed";

export type GithubWorkflowSubject<
  Owner extends string = string,
  Repo extends string = string,
  Workflow extends string = string,
  Action extends GithubWorkflowAction = GithubWorkflowAction,
> = `notifications.github.${SanitizedSubjectSegment<Owner>}.${SanitizedSubjectSegment<Repo>}.workflow.${SanitizedSubjectSegment<Workflow>}.${Action}`;

export function githubWorkflowSubject<
  Owner extends string,
  Repo extends string,
  Workflow extends string,
  Action extends GithubWorkflowAction,
>(
  owner: Owner,
  repo: Repo,
  workflowFilename: Workflow,
  action: Action
): GithubWorkflowSubject<Owner, Repo, Workflow, Action>;
export function githubWorkflowSubject(
  owner: string,
  repo: string,
  workflowFilename: string,
  action: GithubWorkflowAction
) {
  return `${githubRepositoryPrefix(owner, repo)}.workflow.${sanitizeSubjectSegment(workflowFilename)}.${action}`;
}

export type GithubJobAction = "completed";

/**
 * A GitHub Actions job on the repository's default branch, of a run with no pull request, published
 * when it finishes: Envoy reads one from a `check_run` webhook whose App is GitHub Actions, whose
 * check suite's `head_branch` is the repository's `default_branch` and whose `details_url` names the
 * run and the job. The run id is the subject's segment, so a consumer follows one run's jobs as they
 * end.
 */
export type GithubJobSubject<
  Owner extends string = string,
  Repo extends string = string,
  RunId extends string | number = string | number,
  Action extends GithubJobAction = GithubJobAction,
> = `notifications.github.${SanitizedSubjectSegment<Owner>}.${SanitizedSubjectSegment<Repo>}.job.${SanitizedSubjectSegment<`${RunId}`>}.${Action}`;

export function githubJobSubject<
  Owner extends string,
  Repo extends string,
  RunId extends string | number,
  Action extends GithubJobAction,
>(
  owner: Owner,
  repo: Repo,
  runId: RunId,
  action: Action
): GithubJobSubject<Owner, Repo, RunId, Action>;
export function githubJobSubject(
  owner: string,
  repo: string,
  runId: string | number,
  action: GithubJobAction
) {
  return `${githubRepositoryPrefix(owner, repo)}.job.${sanitizeSubjectSegment(String(runId))}.${action}`;
}

export const GHOSTWISPR_TOPIC_PREFIX = "notifications.ghostwispr." as const;

export type GhostWisprSubject<
  SessionId extends string = string,
  Kind extends string = string,
> = `${typeof GHOSTWISPR_TOPIC_PREFIX}${SessionId}.${Kind}`;

export function ghostWisprSubject<SessionId extends string, Kind extends string>(
  sessionId: SessionId,
  kind: Kind
): GhostWisprSubject<SessionId, Kind>;
export function ghostWisprSubject(sessionId: string, kind: string) {
  return `${GHOSTWISPR_TOPIC_PREFIX}${sessionId}.${kind}`;
}

export type WhatsappSubject<
  Phone extends string = string,
  Jid extends string = string,
  Kind extends string = string,
> = `notifications.whatsapp.${Phone}.${Jid}.${Kind}`;

export function whatsappSubject<Phone extends string, Jid extends string, Kind extends string>(
  phone: Phone,
  jid: Jid,
  kind: Kind
): WhatsappSubject<Phone, Jid, Kind>;
export function whatsappSubject(phone: string, jid: string, kind: string) {
  return `notifications.whatsapp.${phone}.${jid}.${kind}`;
}

export type Subject =
  | LegionNoticeSubject
  | AgentSubject
  | DispatchIssueSubject
  | DispatchDocumentSubject
  | GithubSubject
  | GithubResourceSubject
  | GithubPushSubject
  | GithubWorkflowSubject
  | GithubJobSubject
  | SlackSubject
  | SlackThreadSubject
  | GhostWisprSubject
  | WhatsappSubject;

/**
 * What a caller that named `topic` is told when nothing would ever be delivered to `sessionID` on
 * it: a `notifications.` topic no stream subject captures and no role lane
 * (`bus.ReachesNoSession` in packages/envoy/internal/bus, `streamDeliversNothing` in
 * `@legion/envoy-client`). The listener leaves such a topic out of a registration and names it in
 * this warning (`splitUncapturedTopics` in packages/envoy/cmd/listener/api.go, which names it with
 * `contracts.UncapturedTopicRefusal` `scripts/gen-go.ts` emits from this builder), and each
 * plugin's explicit subscribe refuses such a topic in the same words (`envoy_subscribe` in
 * pi-envoy, `follow` in claude-envoy, both through `uncapturedTopicRefusal` in
 * `@legion/envoy-client`). So a caller reads one sentence wherever it is told: a registration
 * carries topics nobody typed — a stored row, a restored transcript — and is answered with a
 * warning, where a tool call is a caller asking for this one topic by name.
 */
export function uncapturedTopicRefusal(topic: string, sessionID: string): string {
  return `${topic} was not registered: no stream subject captures it, so nothing would be delivered to ${sessionID} on it; follow a notifications.team.<name> topic instead`;
}
