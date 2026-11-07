// Every request the dashboard makes is named by the route pattern the Dispatch route table holds
// it under (`/api/v1/issues/{key}/events`), and its path parameters, its query parameters, its
// request body and its response are read from `paths` in `@legion/contracts` - the module
// generated from that table. Nothing here restates a wire shape: a route the table no longer
// holds, a path parameter it renamed, a query parameter it dropped and a response field it
// changed each stop the type checker at the call site that asks for them.
//
// `e2e/api.ts` reads the same helpers, so the two clients cannot drift apart.
import type { paths } from "@legion/contracts";

/** Every route pattern the document holds an `M` operation for ("get", "post", …). A pattern the
 *  document does not hold, or holds no `M` for, is no member, so naming one is an error. */
export type PathsOf<M extends string> = {
  [P in keyof paths]: paths[P] extends { [K in M]: infer O }
    ? O extends never
      ? never
      : P
    : never;
}[keyof paths];

/** The document's operation for one route's `M`. */
export type Op<P extends keyof paths, M extends string> = paths[P] extends { [K in M]: infer O }
  ? O
  : never;

/** The JSON body of an operation's `200`, or `never` when it answers none. */
export type Json200<O> = O extends {
  responses: { 200: { content: { "application/json": infer R } } };
}
  ? R
  : never;

/** The JSON body of an operation's `201`, or `never` when it answers none. */
export type Json201<O> = O extends {
  responses: { 201: { content: { "application/json": infer R } } };
}
  ? R
  : never;

/** What an operation answers a caller that reads its body: its `200`, its `201`, or both where it
 *  answers either (a create whose `201` carries the server's write advice). `undefined` for an
 *  operation whose only success is a `204`, which carries no body — which is what the transport
 *  resolves with for an empty answer. */
export type Answer<O> = [Json200<O> | Json201<O>] extends [never]
  ? undefined
  : Json200<O> | Json201<O>;

/** An operation's path parameters, under the names the document gives them; `{}` for a route
 *  pattern that holds none. The `object &` is what lets `routeUrl` read the record's entries
 *  while `P` is still a type parameter: a path parameter record is always an object. */
export type PathParams<O> = object &
  (O extends { parameters: { path: infer P } }
    ? P extends undefined
      ? Record<never, never>
      : P
    : Record<never, never>);

/** An operation's query parameters; `{}` for an operation that takes none. `object &` reads as
 *  it does on `PathParams`. */
export type QueryParams<O> = object &
  (O extends { parameters: { query?: infer Q } }
    ? [Q] extends [never]
      ? Record<never, never>
      : NonNullable<Q>
    : Record<never, never>);

type JsonBodyOf<R> = [NonNullable<R>] extends [never]
  ? undefined
  : NonNullable<R> extends { content: { "application/json": infer B } }
    ? B
    : undefined;

/** The JSON request body an operation takes: the body's own type, `| undefined` where the
 *  document marks the body optional, and `undefined` for an operation that takes none. */
export type RequestBody<O> = O extends { requestBody: infer R }
  ? JsonBodyOf<R>
  : O extends { requestBody?: infer R }
    ? JsonBodyOf<R> | undefined
    : undefined;

/** `FormData` for an operation the document gives a `multipart/form-data` body (the three artifact
 *  uploads), and `never` for every other, which therefore takes none. */
export type FormBody<O> = O extends {
  requestBody: { content: { "multipart/form-data": unknown } };
}
  ? FormData
  : never;

/** One route's query record, named by the route and the verb: what a caller of that route hands
 *  over as its query. */
export type Query<P extends keyof paths, M extends string> = QueryParams<Op<P, M>>;

/** One route's JSON request body, named by the route and the verb. */
export type Body<P extends keyof paths, M extends string> = RequestBody<Op<P, M>>;

/** What one route answers a caller that reads its body, named by the route and the verb. */
export type Result<P extends keyof paths, M extends string> = Answer<Op<P, M>>;

/** The one way a value enters a path: a parameter is encoded, never interpolated raw. */
function pathSegment(value: string): string {
  return encodeURIComponent(value);
}

function pathWithQuery(path: string, values: object): string {
  const query = new URLSearchParams();

  for (const [key, value] of Object.entries(values)) {
    if (Array.isArray(value)) {
      for (const item of value) {
        query.append(key, item);
      }
    } else if (
      typeof value === "string" ||
      typeof value === "number" ||
      typeof value === "boolean"
    ) {
      query.set(key, String(value));
    }
  }

  const search = query.toString();
  return search ? `${path}?${search}` : path;
}

/** The URL one request goes to: `pattern` with each `{name}` replaced by `params`' value for it,
 *  encoded (`pathSegment`), and `query` appended (`pathWithQuery`). A build that would not match
 *  the pattern throws, naming the pattern and the parameter: a parameter with no value, or an
 *  empty one, would leave a path addressing another route or none, and a parameter the pattern
 *  does not hold would go nowhere. */
export function routeUrl(pattern: string, params: object, query?: object): string {
  const values = new Map(Object.entries(params));
  const path = pattern.replace(/\{([^}]+)\}/gu, (_whole, name: string) => {
    const value = values.get(name);
    if (value === undefined || value === null || value === "") {
      throw new Error(`${pattern} has no value for {${name}}`);
    }
    values.delete(name);
    return pathSegment(String(value));
  });
  if (values.size !== 0) {
    throw new Error(`${pattern} holds no {${values.keys().next().value}}`);
  }
  return query === undefined ? path : pathWithQuery(path, query);
}

/** `owner/name` as the two path parameters every route that addresses a repository names it in.
 *  Anything else throws: a repository that is not exactly two non-empty segments would build a
 *  URL addressing another repository or none. */
export function repoParams(repo: string): { owner: string; repo: string } {
  const [owner, name, ...rest] = repo.split("/");
  if (owner === "" || name === "" || rest.length !== 0) {
    throw new Error("repository must be formatted as owner/repo");
  }
  return { owner, repo: name };
}
