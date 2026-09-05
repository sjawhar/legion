// Turn a validated tool call into the arguments the service needs: the repo
// the working directory implies (when the call does not name one) and the
// origin — machine, cwd, tmux, plus the host and session identity the calling
// plugin read from its host. Every host plugin runs this before callDispatch.

import {
  type DispatchCall,
  DispatchArgumentError,
  type DispatchQuestion,
  type DispatchUrgency,
  isContinueCall,
} from "./dispatch-contract";
import {
  type DispatchHost,
  type DispatchOrigin,
  type ExecFn,
  resolveCwdRepo,
  resolveOrigin,
} from "./dispatch-cwd";

export interface PrepareDispatchCallInput {
  readonly call: DispatchCall;
  readonly cwd: string;
  readonly host: DispatchHost;
  readonly sessionId?: string;
  readonly sessionTitle?: string;
  readonly env: Record<string, string | undefined>;
  readonly exec: ExecFn;
}

/** What goes over the wire as `params.arguments`. */
export interface DispatchServiceArguments {
  readonly subject?: string;
  readonly thread?: string;
  readonly context: string;
  readonly question: string;
  readonly ask?: readonly DispatchQuestion[];
  readonly urgency?: DispatchUrgency;
  readonly repo?: string;
  readonly parent?: string;
  readonly origin: DispatchOrigin;
}

/** "owner/name#123" (optionally "#456" for a parent's comment id). */
const QUALIFIED_REF = /^[^/\s#]+\/[^/\s#]+#\d+/;

export async function prepareDispatchCall(
  input: PrepareDispatchCallInput
): Promise<DispatchServiceArguments> {
  const { call, cwd } = input;
  const continuing = isContinueCall(call);
  const needsRepo = continuing
    ? !QUALIFIED_REF.test(call.thread)
    : call.repo === undefined && !QUALIFIED_REF.test(call.parent ?? "");
  let repo: string | undefined;
  if (needsRepo) {
    const resolved = await resolveCwdRepo(cwd, input.exec);
    if (resolved === null) {
      throw new DispatchArgumentError(
        continuing
          ? `dispatch: ${cwd} has no GitHub remote; pass thread=owner/name#<n>`
          : `dispatch: ${cwd} has no GitHub remote; pass repo=owner/name`
      );
    }
    repo = resolved;
  }
  const resolvedOrigin = await resolveOrigin(input.env, input.exec, cwd);
  const origin: DispatchOrigin = {
    ...resolvedOrigin,
    host: input.host,
    ...(input.sessionId ? { sessionId: input.sessionId } : {}),
    ...(input.sessionTitle ? { sessionTitle: input.sessionTitle } : {}),
  };
  return { ...call, ...(repo === undefined ? {} : { repo }), origin };
}
