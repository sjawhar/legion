import { api, type WriteBounds, type WriteMethod, type WriteRequest } from "../api/client";
import type {
  CreateAgentMessageInput,
  UpdateIssueInput,
  UserAgentStateInput,
  UserIssueState,
} from "../api/types";

/** One kind of plain write on a line: the route its builder in `api/client.ts` names, the client
 *  method a test spies on to answer it, and that method called with what the request carries. */
interface LineWrite {
  readonly method: WriteMethod;
  readonly route: RegExp;
  readonly client: keyof typeof api;
  readonly call: (segment: string, body: unknown, bounds: WriteBounds) => Promise<unknown>;
}

/** `body`'s string `field`, which the builder of the request it came from always sets. */
function stringField(body: unknown, field: string): string {
  const value: unknown = typeof body === "object" && body !== null ? Reflect.get(body, field) : "";
  if (typeof value !== "string") throw new TypeError(`a line write's body has no ${field}`);
  return value;
}

/** Every request builder a plain write on a line is made from (`issuePatchRequest` …), read
 *  back into its client method. The bodies are the builders' own, which the page typed. */
const LINE_WRITES: readonly LineWrite[] = [
  {
    call: (key, body, bounds) => api.patchIssue(key, body as UpdateIssueInput, bounds),
    client: "patchIssue",
    method: "PATCH",
    route: /^\/api\/v1\/issues\/([^/]+)$/u,
  },
  {
    call: (session, body, bounds) =>
      api.createAgentMessage(session, body as CreateAgentMessageInput, bounds),
    client: "createAgentMessage",
    method: "POST",
    route: /^\/api\/v1\/agents\/([^/]+)\/messages$/u,
  },
  {
    call: (id, body, bounds) => api.snoozeAsk(id, stringField(body, "snoozed_until"), bounds),
    client: "snoozeAsk",
    method: "PUT",
    route: /^\/api\/v1\/me\/asks\/([^/]+)\/snooze$/u,
  },
  {
    call: (id, body, bounds) => api.markThreadRead(id, stringField(body, "read_through"), bounds),
    client: "markThreadRead",
    method: "PUT",
    route: /^\/api\/v1\/me\/threads\/([^/]+)\/read$/u,
  },
  {
    call: (key, body, bounds) => api.putIssueState(key, body as Partial<UserIssueState>, bounds),
    client: "putIssueState",
    method: "PUT",
    route: /^\/api\/v1\/me\/issues\/([^/]+)\/state$/u,
  },
  {
    call: (session, body, bounds) =>
      api.putAgentState(session, body as UserAgentStateInput, bounds),
    client: "putAgentState",
    method: "PUT",
    route: /^\/api\/v1\/me\/agents\/([^/]+)\/state$/u,
  },
];

/**
 * A plain write on a line goes out as `api.send` of the request its press named (`Writes.run`),
 * never through the client method that makes the same request (`patchIssue`). A unit test answers
 * a write by spying on that method, as it answers every other: so while one is spied on, `api.send`
 * of a request its builder names calls the spy with what the request carries - its key or id and
 * its body - and the test sees the call it would see from the method itself. A method that is not
 * spied on is never called here (it would call `api.send` again), and every other request goes on
 * to the real `api.send`. A test that spies on `api.send` itself takes over from this.
 */
export function answerLineWritesByMethod(): void {
  const send = api.send.bind(api);
  api.send = (<T>(request: WriteRequest<T>, bounds: WriteBounds): Promise<T> => {
    for (const line of LINE_WRITES) {
      const segment =
        request.method === line.method ? line.route.exec(request.path)?.[1] : undefined;
      const client: unknown = api[line.client];
      if (segment === undefined || typeof client !== "function" || !("mock" in client)) continue;
      return line.call(decodeURIComponent(segment), request.body, bounds) as Promise<T>;
    }
    return send(request, bounds);
  }) as typeof api.send;
}
