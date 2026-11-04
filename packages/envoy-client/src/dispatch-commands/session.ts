import type { InboxRow, SessionAnswersResponse, WhoamiResponse } from "@legion/contracts";
import { serviceSubjectLabel } from "@legion/contracts";
import { textHead } from "../ask-answer";
import { type CredentialPendingRow, DispatchServiceError } from "../dispatch-http";
import { dispatchChildRef, dispatchDocumentRef, dispatchIssueRef } from "../dispatch-owner";
import { messageFor } from "../errors";
import { isNoAnswersRoute, renderSessionAnswersList } from "../session-answers";
import type { DispatchToolResult, SessionContext } from "./context";
import { dispatchAnswered, refusalWithCode } from "./failures";

/**
 * `dispatch answers`: the answers this session's asks have been given since it last read them.
 * It names no owner, so it runs on the session's context alone.
 */
export async function answersCommand(context: SessionContext): Promise<DispatchToolResult> {
  const { configUrl, input, newClient: dispatchClient } = context;
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch answers");
  let response: SessionAnswersResponse;
  try {
    response = await dispatchClient().sessionAnswers(sessionId);
  } catch (error) {
    if (isNoAnswersRoute(error)) {
      throw new Error(
        `This Dispatch (${configUrl}) has no answers route yet: its build is older than the answers route.`
      );
    }
    throw error;
  }
  return {
    text: renderSessionAnswersList(response.items, { now: Date.now() }),
    details: { session: sessionId, since: response.since, total: response.total },
  };
}

/** The person a credential acts for, as `GET /api/v1/whoami` names them: the owner of a personal
 *  or broker-issued token, and a signed-in person's own login in the canonical lowercase every
 *  stored login and assignee carries. `null` for a credential that acts for nobody, which the
 *  shared token and a service-account token are. */
function whoamiPerson(identity: WhoamiResponse): string | null {
  return identity.kind === "agent" ? identity.owner : identity.login.toLowerCase();
}

/**
 * `dispatch whoami`: the identity Dispatch records this session's writes as. It names no owner,
 * so it runs on the session's context alone.
 */
export async function whoamiCommand(context: SessionContext): Promise<DispatchToolResult> {
  const { input, newClient: dispatchClient } = context;
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch whoami");
  const client = dispatchClient();
  const identity = await client.whoami();
  const owner = whoamiPerson(identity);
  // A Dispatch that predates verified service tokens omits the field entirely.
  const service = identity.kind === "agent" ? (identity.service ?? null) : null;
  // So does one that predates broker-issued tokens, which are the only credential that proves
  // a session of its own: read either omission as that older build, not as "no session".
  const proved = identity.kind === "agent" ? (identity.session ?? null) : null;
  const project = identity.kind === "agent" ? (identity.project ?? null) : null;
  // The session Dispatch records the writes as: the one the credential proved, else this host's.
  const session = proved ?? sessionId;
  const unowned =
    "no owner, so an issue you create without an assignee takes its parent's assignee, else the project's default assignee, else nobody.";
  const acting =
    owner !== null
      ? `Session ${session} acts for ${owner}: issues you create without an assignee are assigned to ${owner}.`
      : service !== null
        ? // The text names the identity the way every other surface does; details
          // keeps the subject whole, because that is the persisted value.
          `Session ${session} runs as service ${serviceSubjectLabel(service)}: ${unowned}`
        : `Session ${session} runs under the shared token: ${unowned}`;
  const elsewhere =
    proved !== null && proved !== sessionId
      ? ` Your credential names session ${proved}, which is not this host session (${sessionId}): Dispatch records every write you make as ${proved}.`
      : "";
  const writable = project !== null ? ` It may write project ${project} and no other.` : "";
  return {
    text: acting + elsewhere + writable,
    details: { session, owner, service, project },
  };
}

/** How much of an ask's question the Inbox summary's line carries. The event summaries' head
 *  (`textHead`) is longer: a question is one line among four counted bands here, and the whole of
 *  it is one `dispatch read` away. */
const INBOX_QUESTION_HEAD = 80;

/**
 * Whether the person has deferred this row and its moment is still ahead: the Inbox page's own
 * `isSnoozed` (`packages/dispatch/web/src/features/inbox/snooze.ts`), re-read here and held to
 * the page by Dispatch's browser test (`packages/dispatch/e2e/inbox-cli.e2e.ts`), which compares
 * this command's bands with the page's. The window is absolute, so nothing an agent does to the
 * ask shortens it; a row Dispatch lists with no snooze at all is not deferred, and neither is one
 * whose moment has passed.
 */
function inboxSnoozed(row: InboxRow, now: number): boolean {
  if (row.snoozed_until === null) return false;
  const at = Date.parse(row.snoozed_until);
  return !Number.isNaN(at) && at > now;
}

/**
 * Which band a row sits in: the page's `sectionOf` (`.../features/inbox/sections.ts`). A snooze
 * the person set wins over everything, because deferring an ask is them saying it does not need
 * them now; every other row sits under whose turn it is. A row the Inbox lists without a turn
 * belongs to no band, as on the page, and so is listed under no heading.
 */
function inboxSection(row: InboxRow, now: number): "agent" | "human" | "later" | undefined {
  if (inboxSnoozed(row, now)) return "later";
  return row.waiting_on;
}

/** Where an inbox row's ask lives: the issue key or the document slug its line is named by, and
 *  the ask's own `dispatch://` address under that owner. Every row the Inbox lists has one owner
 *  or the other; a row with neither, which no read makes, is named by the ask's id alone. */
function inboxAskOwner(row: InboxRow): { readonly label: string; readonly ref: string } {
  const key = row.issue?.key ?? row.issue_key;
  if (key !== null) {
    return { label: key, ref: dispatchChildRef(dispatchIssueRef(key), "ask", row.id) };
  }
  const document = row.document;
  if (document !== undefined) {
    return {
      label: document.slug,
      ref: dispatchChildRef(dispatchDocumentRef(document.project, document.slug), "ask", row.id),
    };
  }
  return { label: row.id, ref: row.id };
}

function inboxAskLine(row: InboxRow): string {
  const { label, ref } = inboxAskOwner(row);
  const priority = row.priority === null ? "P-" : `P${row.priority}`;
  return `- ${label} · ${priority} · ${row.kind} · ${textHead(row.question, INBOX_QUESTION_HEAD)} · ${ref}`;
}

/** One pending credential request: what it asks for, when, the sessions it names and the page
 *  where it is decided. A row that names no session leaves that field out rather than printing
 *  two dashes, and a machine login, which is decided with `dispatch` rather than on a broker
 *  page, carries no url to print. */
function credentialRequestLine(request: CredentialPendingRow): string {
  const session = request.session ?? null;
  return [
    `- ${request.kind}`,
    request.identifiers.join(", "),
    request.requested_at,
    ...(session === null
      ? []
      : [`session request=${session.request ?? "-"}, enrollment=${session.enrollment ?? "-"}`]),
    ...(request.approve_url === undefined ? [] : [request.approve_url]),
  ].join(" · ");
}

/**
 * What the person's Inbox page shows, as the `dispatch inbox` command prints it: each heading
 * with its count, then the rows under it, in the order the page bands them. The rows within a
 * band are the server's order and nothing re-sorts them.
 *
 * `Needs you` is the page's `needsYou` (`.../features/inbox/BlockedOnYou.tsx`): the asks waiting
 * on a person that the viewer has not deferred and that sit on their own issues (`waitingOnYou`),
 * plus every credential request, which is the viewer's by construction because the list is read
 * as `?approver=me`. Here that is the band's own rows and nothing narrower, since `?assignee=me`
 * keeps only the asks on issues assigned to the caller, as the page's default view does; counting
 * the rows it prints is therefore the page's rule, and it is the one count that cannot disagree
 * with the lines under it.
 */
function formatInboxSummary(
  rows: readonly InboxRow[],
  requests: readonly CredentialPendingRow[],
  now: number
): string {
  const sections = { agent: [] as InboxRow[], human: [] as InboxRow[], later: [] as InboxRow[] };
  for (const row of rows) {
    const section = inboxSection(row, now);
    if (section !== undefined) sections[section].push(row);
  }
  return [
    `Needs you ${sections.human.length + requests.length}`,
    ...sections.human.map(inboxAskLine),
    "",
    `Waiting on agents ${sections.agent.length}`,
    ...sections.agent.map(inboxAskLine),
    "",
    `Later ${sections.later.length}`,
    ...sections.later.map(inboxAskLine),
    "",
    `Credential requests ${requests.length}`,
    ...requests.map(credentialRequestLine),
  ].join("\n");
}

/**
 * `dispatch inbox`: what the person's Inbox page shows. It names no owner, so it runs on the
 * session's context alone.
 */
export async function inboxCommand(context: SessionContext): Promise<DispatchToolResult> {
  const client = context.newClient();
  // All three reads at once: they depend on nothing of each other, and the credential list is
  // the slow one, since Dispatch relays it to the secrets broker. Its rejection is caught at
  // the start so that a refusal of the other two, which throws first, leaves no unhandled one.
  const pending = client.credentialPending().then(
    (answer) => ({ answer }),
    (error: unknown) => ({ error })
  );
  let identity: WhoamiResponse;
  let rows: InboxRow[];
  try {
    [identity, rows] = await Promise.all([client.whoami(), client.inbox("me")]);
  } catch (error) {
    // The inbox route needs `person`, and a credential without it is refused 403
    // OAUTH_SCOPE_REQUIRED, whose message names the scope: the code is what the caller acts
    // on, so it reaches the text. `whoami` needs no scope.
    throw refusalWithCode(error);
  }
  const viewer = whoamiPerson(identity);
  const settled = await pending;
  let requests: readonly CredentialPendingRow[] = [];
  const warnings: string[] = [];
  if ("error" in settled) {
    const error = settled.error;
    // The asks are the greater part of the Inbox, so one failed read of the broker's list does
    // not cost the caller the rest of it. A refusal for want of a scope is the exception: the
    // credential itself is wrong, and reporting that as a missing section would hide it.
    if (dispatchAnswered(error, 403) && error.code === "OAUTH_SCOPE_REQUIRED") {
      throw refusalWithCode(error);
    }
    const refusal =
      error instanceof DispatchServiceError ? `${error.code} ${error.message}` : messageFor(error);
    warnings.push(`dispatch: warning: credential requests could not be read: ${refusal}`);
  } else {
    // A Dispatch with no secrets broker answers null, which is an ordinary deployment: it has
    // no credential requests, so it lists and counts none.
    requests = settled.answer?.pending ?? [];
  }
  return {
    text: formatInboxSummary(rows, requests, Date.now()),
    details: {
      viewer,
      asks: rows.length,
      credential_requests: requests.length,
    },
    ...(warnings.length === 0 ? {} : { warnings }),
  };
}
