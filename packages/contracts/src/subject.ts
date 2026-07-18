export const AGENT_TOPIC_PREFIX = "notifications.agent." as const;
export const ROLE_TOPIC_PREFIX = "notifications.role." as const;

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
> = `notifications.github.${Owner}.${Repo}.${Kind}`;

export function githubSubject<Owner extends string, Repo extends string, Kind extends string>(
  owner: Owner,
  repo: Repo,
  kind: Kind
): GithubSubject<Owner, Repo, Kind>;
export function githubSubject(owner: string, repo: string, kind: string) {
  return `notifications.github.${owner}.${repo}.${kind}`;
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
 * Replaces dots in a NATS subject segment with underscores so the segment
 * stays a single token. Mirrored on the Go side as `SanitizeSubjectSegment`.
 *
 * Note: this is intentionally lossy; subscribers needing the exact identifier
 * should inspect the envelope payload.
 */
export function sanitizeSubjectSegment(value: string): string {
  return value.replaceAll(".", "_");
}

type ReplaceDotsWithUnderscores<Value extends string> = Value extends `${infer Head}.${infer Tail}`
  ? `${Head}_${ReplaceDotsWithUnderscores<Tail>}`
  : Value;

export type SlackThreadSubject<
  Team extends string = string,
  Channel extends string = string,
  ThreadTs extends string = string,
  Kind extends string = string,
> = `notifications.slack.${Team}.${Channel}.thread.${ReplaceDotsWithUnderscores<ThreadTs>}.${Kind}`;

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
> = `notifications.github.${Owner}.${Repo}.${ResourceType}.${ResourceNumber}`;

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
  return `notifications.github.${owner}.${repo}.${resourceType}.${resourceNumber}`;
}

export type GithubPushRefType = "branch" | "tag";

export type GithubPushSubject<
  Owner extends string = string,
  Repo extends string = string,
  RefType extends GithubPushRefType = GithubPushRefType,
  RefName extends string = string,
> = `notifications.github.${Owner}.${Repo}.push.${RefType}.${ReplaceDotsWithUnderscores<RefName>}`;

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
  return `notifications.github.${owner}.${repo}.push.${refType}.${sanitizeSubjectSegment(refName)}`;
}

export type GithubWorkflowAction = "requested" | "in_progress" | "completed";

export type GithubWorkflowSubject<
  Owner extends string = string,
  Repo extends string = string,
  Workflow extends string = string,
  Action extends GithubWorkflowAction = GithubWorkflowAction,
> = `notifications.github.${Owner}.${Repo}.workflow.${ReplaceDotsWithUnderscores<Workflow>}.${Action}`;

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
  return `notifications.github.${owner}.${repo}.workflow.${sanitizeSubjectSegment(workflowFilename)}.${action}`;
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
  | AgentSubject
  | GithubSubject
  | GithubResourceSubject
  | GithubPushSubject
  | GithubWorkflowSubject
  | SlackSubject
  | SlackThreadSubject
  | GhostWisprSubject
  | WhatsappSubject;
