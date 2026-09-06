// The dispatch pipeline every host plugin runs once its model has called the
// tool: turn the validated call into the arguments the service needs — the
// repo the working directory implies (when the call does not name one) and the
// origin: machine, cwd, tmux, plus the host and session identity the plugin
// read from its host — then post it. Hosts keep only their identity lookup and
// their result formatting.

import {
  callDispatch,
  type DispatchServiceResult,
  ghTokenGetter,
  type TokenGetter,
} from "./dispatch-client";
import {
  DispatchArgumentError,
  type DispatchCall,
  type DispatchQuestion,
  type DispatchUrgency,
  isContinueCall,
} from "./dispatch-contract";
import {
  type DispatchHost,
  type DispatchOrigin,
  defaultExec,
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

/** What goes over the wire as `params.arguments`. Optional members carry `undefined` because the call they spread from does. */
export interface DispatchServiceArguments {
  readonly subject?: string | undefined;
  readonly thread?: string | undefined;
  readonly context: string;
  readonly question: string;
  readonly ask?: readonly DispatchQuestion[] | undefined;
  readonly urgency?: DispatchUrgency | undefined;
  readonly repo?: string | undefined;
  readonly parent?: string | undefined;
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

export interface ExecuteDispatchInput {
  /** The validated call: hosts run parseDispatchCall first, before any identity lookup of their own. */
  readonly call: DispatchCall;
  readonly cwd: string;
  readonly host: DispatchHost;
  /** Optional members admit `undefined` so a host can pass `value || undefined` under exactOptionalPropertyTypes. */
  readonly sessionId?: string | undefined;
  readonly sessionTitle?: string | undefined;
  /** The service's /mcp endpoint, from resolveDispatchConfig(). */
  readonly serviceUrl: string;
  readonly env?: Record<string, string | undefined>;
  readonly exec?: ExecFn;
  /** Defaults to `gh auth token` in `cwd` through `exec`. */
  readonly getToken?: TokenGetter;
  readonly fetchImpl?: typeof fetch;
}

/** prepareDispatchCall then callDispatch, with the production defaults for everything a host does not inject. */
export async function executeDispatch(input: ExecuteDispatchInput): Promise<DispatchServiceResult> {
  const exec = input.exec ?? defaultExec;
  const prepared = await prepareDispatchCall({
    call: input.call,
    cwd: input.cwd,
    host: input.host,
    ...(input.sessionId === undefined ? {} : { sessionId: input.sessionId }),
    ...(input.sessionTitle === undefined ? {} : { sessionTitle: input.sessionTitle }),
    env: input.env ?? process.env,
    exec,
  });
  return callDispatch(
    {
      serviceUrl: input.serviceUrl,
      getToken: input.getToken ?? ghTokenGetter(input.cwd, exec),
      ...(input.fetchImpl ? { fetchImpl: input.fetchImpl } : {}),
    },
    prepared
  );
}
