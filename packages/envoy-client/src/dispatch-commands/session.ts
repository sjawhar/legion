import type { SessionAnswersResponse } from "@legion/contracts";
import { serviceSubjectLabel } from "@legion/contracts";
import { isNoAnswersRoute, renderSessionAnswersList } from "../session-answers";
import type { DispatchToolResult, SessionContext } from "./context";

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
  const owner = identity.kind === "agent" ? identity.owner : identity.login.toLowerCase();
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
