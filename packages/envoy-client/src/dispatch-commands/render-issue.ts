import type {
  Event,
  Issue,
  IssueClaim,
  IssueComponents,
  IssueCoverage,
  IssueDetails,
  IssueProgress,
  IssueReferences,
  IssueRouteReach,
  IssueRouteStatus,
} from "@legion/contracts";
import { actorLabel, claimHolds } from "@legion/contracts";
import { actorText } from "../actor-text";
import { askAnswerText, textHead } from "../ask-answer";
import type { DispatchClient } from "../dispatch-http";
import { humanEditHead } from "../human-edit";
import { approvalLine, documentListLines } from "./document-targets";

/** The Components line of an issue read: the effective set with where it came from. */
function componentsLine(components: IssueComponents): string {
  const inherited =
    components.inherited_from === null ? "" : ` (inherited from ${components.inherited_from})`;
  const retired =
    components.unknown.length === 0 ? "" : ` (retired: ${components.unknown.join(", ")})`;
  switch (components.mode) {
    case "explicit":
      return `Components: ${components.ids.length === 0 ? "none live" : components.ids.join(", ")}${inherited}${retired}`;
    case "none":
      return `Components: none — ${components.reason ?? ""}${inherited}`;
    case "inherit":
      return "Components: unassigned";
  }
}

/**
 * How every agent surface names a claim's holder and says whether the claim still holds. The
 * name is `actorLabel` from `@legion/contracts`, the same function the dashboard's header, List
 * rows and Board cards call, so a session reading `dispatch_read` and a human reading the issue
 * page see one name for one holder; without a registry it falls back to the title the session
 * stamped on the claim, as the dashboard's does. Whether it holds is `claimHolds`, the judgement
 * the dashboard's claim chip makes too: a session the loaded registry does not list reads
 * "· not running", as the chip does, and its issue is free to claim; with no registry (`titles`
 * undefined) a session's claim reads "· liveness unknown", since nothing can say whether that
 * session runs. A person's claim, and a session the registry lists, carry no marker.
 */
export function claimText(
  claim: IssueClaim,
  titles: ReadonlyMap<string, string> | undefined
): string {
  const holding = claimHolds(claim, titles);
  const marker =
    holding === "unknown" ? " · liveness unknown" : holding === "lapsed" ? " · not running" : "";
  return `${actorLabel(claim.actor, titles)} since ${claim.at}${marker}`;
}

/**
 * The live agent registry behind one piece of output, as session titles by id: at most one
 * `GET /api/v1/agents` per tool call, never per row, and none at all unless a session holds
 * something being rendered — the same gate the dashboard applies with `useAgents(holdsSession)`.
 *
 * `undefined` is no registry: none was needed, or the agents request failed. Names then fall back
 * to the title each session stamped on its write, and a session's claim reads "liveness unknown"
 * (`claimText`) rather than running or not running. A holder the registry does not list is the
 * ordinary case, a session that has ended, whose claim the next agent may take.
 */
export async function liveSessionTitles(
  client: DispatchClient,
  needed: boolean
): Promise<ReadonlyMap<string, string> | undefined> {
  if (!needed) {
    return undefined;
  }
  try {
    const agents = await client.listAgents();
    return new Map(agents.map((agent) => [agent.session_id, agent.title]));
  } catch {
    return undefined;
  }
}

/** Whether a claim needs the live registry to be named: only a session has a title that moves. */
export function holdsSession(claim: IssueClaim | null | undefined): boolean {
  return claim?.actor.kind === "session";
}

export type RoutedIssue = Pick<Issue, "route"> & Partial<IssueRouteReach>;

/** Whether the route is a role a live session holds, which the live registry names. */
export function routeHeldBySession(issue: RoutedIssue): boolean {
  return issue.route_status === "live" && issue.route?.startsWith("role:") === true;
}

/**
 * An issue's route as every agent surface reads it: where its messages go and whether that
 * reaches anyone, from the `route_status` the server resolved on this read. A route to a role
 * nobody holds, or to a session that is not running, reaches nobody as of the listener's session
 * list Dispatch last stored, at most 5 s old and the same on every server process, so it says
 * so - and says "right now", because a restarting session is absent for minutes.
 */
export function routeText(issue: RoutedIssue, titles?: ReadonlyMap<string, string>): string {
  if (issue.route === null) return "none";
  const holder = issue.route_holder ?? null;
  const reach: Record<IssueRouteStatus, string> = {
    live:
      routeHeldBySession(issue) && holder !== null
        ? ` (held by ${titles?.get(holder) ?? holder})`
        : "",
    no_holder: issue.route.startsWith("role:")
      ? " (nobody holds it right now)"
      : " (that session is not running right now)",
    unknown: " (the Envoy listener did not answer, so whether it reaches anyone is unknown)",
  };
  return issue.route + (issue.route_status == null ? "" : reach[issue.route_status]);
}

/**
 * One issue's progress as the tools print it: `tasks 3/7, children 2/5`, each part only when the
 * server counted it (`null` is a spec with no task list, an issue with no child), joined by sep.
 * Empty when neither counted.
 */
export function progressText(progress: IssueProgress | undefined, sep: string): string {
  if (progress === undefined) return "";
  const parts: string[] = [];
  if (progress.tasks !== null) parts.push(`tasks ${progress.tasks.done}/${progress.tasks.total}`);
  if (progress.children !== null) {
    parts.push(`children ${progress.children.done}/${progress.children.total}`);
  }
  return parts.join(sep);
}

/** Which repository the issue targets and where it came from: its own, or resolved through its
 *  parent chain and the project's default mapping. A Dispatch server from before the repository
 *  fields sends neither field, and its answer is unknown, never none: none means the server
 *  resolved no repository. */
function repositoryLine(issue: {
  readonly repo?: string | null;
  readonly resolved_repo?: string | null;
}): string {
  if (issue.resolved_repo === undefined) {
    return "Repository: unknown (this Dispatch server predates repository fields; upgrade it)";
  }
  if (issue.resolved_repo === null) return "Repository: none";
  if (issue.repo !== null && issue.repo !== undefined) return `Repository: ${issue.repo} (own)`;
  return `Repository: ${issue.resolved_repo} (inherited or project default)`;
}

/** The warning a read from a Dispatch server without the repository fields carries: it names the
 *  refusal this client does not make, so a session's clients can update before its server does. */
export function repositoryFieldsWarnings(issue: {
  readonly key: string;
  readonly resolved_repo?: string | null;
}): { readonly warnings?: readonly string[] } {
  if (issue.resolved_repo !== undefined) return {};
  return {
    warnings: [
      `dispatch: warning: Dispatch issue is missing resolved_repo: this Dispatch server predates repository fields, so ${issue.key}'s repository is unknown; upgrade the server`,
    ],
  };
}
export function issueSummary(
  issue: IssueDetails,
  events: readonly Event[],
  references: IssueReferences | string,
  graph: readonly string[],
  linkBase: string,
  coverage: IssueCoverage | string,
  titles?: ReadonlyMap<string, string>
): string {
  const asks = issue.open_asks;
  const spec = issue.artifacts?.find((artifact) => artifact.primary);
  const specApproval = spec === undefined ? undefined : approvalLine(spec);
  if (issue.priority === undefined) throw new Error("Dispatch issue is missing priority");
  if (issue.assignee === undefined) throw new Error("Dispatch issue is missing assignee");
  if (issue.components === undefined) throw new Error("Dispatch issue is missing components");
  if (issue.claim === undefined) throw new Error("Dispatch issue is missing claim");
  if (issue.external_links === undefined) {
    throw new Error("Dispatch issue is missing external_links");
  }
  return [
    `Title: ${issue.title}`,
    `Key: ${issue.key}`,
    // The issue's page, on the dashboard people open (DISPATCH_PUBLIC_URL when set), so an agent
    // asked about the issue repeats a link rather than building one.
    `Link: ${new URL(`/issues/${encodeURIComponent(issue.key)}`, linkBase).toString()}`,
    `Status: ${issue.status}`,
    `Assignee: ${issue.assignee ?? "unassigned"}`,
    `Claimed by: ${issue.claim === null ? "nobody" : claimText(issue.claim, titles)}`,
    ...(issue.priority === null ? [] : [`Priority: P${issue.priority}`]),
    `Labels: ${issue.labels.length === 0 ? "none" : issue.labels.join(", ")}`,
    componentsLine(issue.components),
    `Route: ${routeText(issue, titles)}`,
    repositoryLine(issue),
    `Progress: ${progressText(issue.progress, ", ") || "none"}`,
    ...(specApproval === undefined
      ? []
      : [`Spec ${specApproval.replace(/^Approval/, "approval")}`]),
    // What the spec's requirements lack: one line per gap, or the one line that says the spec
    // marks none. A gap refuses nothing; it is what the reader fixes before the work starts. A
    // read the server could not answer says so rather than reading as a spec with no gaps.
    ...(typeof coverage === "string"
      ? [`Coverage: ${coverage}`]
      : coverage.gaps.map((gap) => `Coverage: ${gap}`)),
    // The links a person or an agent put on the issue — the pull request that delivers it among
    // them — which the issue page renders with their state; the reference graph below carries
    // only links between Dispatch nodes.
    "External links:",
    ...(issue.external_links.length === 0
      ? ["- none"]
      : issue.external_links.map(
          (link) => `- ${link.url}${link.kind === undefined ? "" : ` (${link.kind})`}`
        )),
    // Every document, picture and file the issue holds, oldest first, which a document
    // reference's not-found hint points to once the issue holds more than the hint lists.
    "Documents:",
    ...documentListLines(issue.artifacts ?? []),
    "Open asks:",
    ...(asks.length === 0 ? ["- none"] : asks.map((ask) => `- ${ask.id}: ${ask.question}`)),
    "References:",
    ...(typeof references === "string"
      ? [`- ${references}`]
      : references.members.length === 0
        ? ["- none"]
        : references.members.map(
            ({ artifact, depth, via }) =>
              `- ${artifact.project}/${artifact.slug} · depth ${depth} via ${via.kind} ${via.id}`
          )),
    ...(typeof references === "string" || !references.truncated
      ? []
      : ["- more references beyond 8 hops"]),
    "Events:",
    ...(events.length === 0 ? ["- none"] : events.map(eventLine)),
    ...graph,
  ].join("\n");
}
export function logSummary(issue: IssueDetails, events: readonly Event[]): string {
  return [
    `Key: ${issue.key}`,
    "Events:",
    ...(events.length === 0 ? ["- none"] : events.map(eventLine)),
  ].join("\n");
}

/** The head of the text an event carries, so a log reads without opening each item. */
function eventHead(event: Event): string | undefined {
  switch (event.type) {
    case "ask.opened":
    case "ask.anchor_refreshed":
    case "ask.edited":
    case "ask.handed_back":
    case "ask.resolved":
      return textHead(event.payload.question);
    case "ask.answered": {
      const previousAnswer = event.payload.previous_answer;
      return `${textHead(event.payload.question)} -> ${textHead(askAnswerText(event.payload.answer))}${previousAnswer === undefined ? "" : ` (was: ${textHead(askAnswerText(previousAnswer))})`}`;
    }
    case "comment.created":
    case "comment.answered":
    case "comment.anchor_refreshed":
    case "comment.edited":
    case "comment.resolved":
    case "comment.reopened":
    case "suggestion.accepted":
    case "suggestion.rejected":
    case "message.created":
    case "message.answered":
      return textHead(event.payload.body);
    case "artifact.version":
      return textHead(
        `${event.payload.name} v${event.payload.version.number}${event.payload.version.summary ? `: ${event.payload.version.summary}` : ""}`
      );
    case "artifact.human_edited":
      return textHead(humanEditHead(event.payload));
    case "issue.created":
    case "issue.updated":
    case "issue.closed":
      return `status ${event.payload.status}`;
    default:
      return undefined;
  }
}

/** The `dispatch://` reference of the message, comment or ask an issue event carries, so a log
 *  line names what `dispatch read --ref` reads and a spec links: the log shows no other id. */
function eventRef(event: Event): string | undefined {
  const payload: unknown = event.payload;
  const id =
    typeof payload === "object" && payload !== null && "id" in payload ? payload.id : undefined;
  if (typeof id !== "string" || id === "" || event.issue_key === null) return undefined;
  const kind = event.type.startsWith("message.")
    ? "message"
    : event.type.startsWith("comment.") || event.type.startsWith("suggestion.")
      ? "comment"
      : event.type.startsWith("ask.")
        ? "ask"
        : undefined;
  return kind === undefined ? undefined : `dispatch://${event.issue_key}/${kind}/${id}`;
}

function eventLine(event: Event): string {
  const head = eventHead(event);
  const ref = eventRef(event);
  return `- #${event.seq} ${event.type}${ref === undefined ? "" : ` ${ref}`} · ${actorText(event.actor)} · ${event.created_at}${head === undefined || head === "" ? "" : ` · ${head}`}`;
}

export function childrenSummary(issue: IssueDetails): string {
  return [
    `Key: ${issue.key}`,
    "Children:",
    ...(issue.children.length === 0
      ? ["- none"]
      : issue.children.map((child) => `- ${child.key}: ${child.title} (${child.status})`)),
  ].join("\n");
}
