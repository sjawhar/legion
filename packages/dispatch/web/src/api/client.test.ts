import { expect, test } from "bun:test";
import { NO_ROUTE_ROLLING_RELEASE_MESSAGE, type Sleep, sleep } from "@legion/contracts";

import { ApiError, createApiClient, type FetchImplementation, isNoRoute } from "./client";

// While a release rolls out, an older Dispatch process answers a route only the newer build serves
// with this 404. No handler ran, so the page sends the request again, and the next attempt may
// reach the newer process.
function noRoute(method: string, path: string): Response {
  return Response.json(
    {
      code: "NOT_FOUND",
      error: `no route for ${method} ${path}`,
      hint: "GET /api/v1 lists every route",
    },
    { status: 404 }
  );
}

/** A fetch that answers each request with the next of `answers`, recording what it was sent. */
function scriptedFetch(answers: readonly (() => Response)[]): {
  fetch: FetchImplementation;
  requests: { path: string; init: RequestInit | undefined }[];
} {
  const requests: { path: string; init: RequestInit | undefined }[] = [];
  return {
    fetch: async (input, init) => {
      requests.push({ path: input.toString(), init });
      const answer = answers[requests.length - 1];
      if (answer === undefined) throw new Error(`unexpected request ${requests.length}`);
      return answer();
    },
    requests,
  };
}

/** A sleep that returns at once and records each wait it was asked for. */
function recordedSleep(): { sleep: (ms: number) => Promise<void>; waits: number[] } {
  const waits: number[] = [];
  return {
    sleep: async (ms) => {
      waits.push(ms);
    },
    waits,
  };
}

test("a no-route answer is sent again, and the next answer resolves the request", async () => {
  const stub = scriptedFetch([
    () => noRoute("GET", "/auth/whoami"),
    () => Response.json({ event_head: "0", kind: "user", login: "alice" }),
  ]);
  const wait = recordedSleep();
  const api = createApiClient(stub.fetch, undefined, wait.sleep);

  expect(await api.whoAmI()).toEqual({ event_head: "0", kind: "user", login: "alice" });
  expect(stub.requests.map((request) => request.path)).toEqual(["/auth/whoami", "/auth/whoami"]);
  // One wait of a second, jittered by a quarter either way.
  expect(wait.waits).toHaveLength(1);
  expect(wait.waits[0]).toBeGreaterThanOrEqual(750);
  expect(wait.waits[0]).toBeLessThanOrEqual(1_250);
});

test("four no-route answers throw the message that a release is in progress", async () => {
  const stub = scriptedFetch(Array.from({ length: 4 }, () => () => noRoute("GET", "/auth/whoami")));
  const wait = recordedSleep();
  const api = createApiClient(stub.fetch, undefined, wait.sleep);

  const error = await api.whoAmI().catch((thrown: unknown) => thrown);
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({
    code: "NOT_FOUND",
    message: NO_ROUTE_ROLLING_RELEASE_MESSAGE,
    status: 404,
  });
  expect(NO_ROUTE_ROLLING_RELEASE_MESSAGE).toBe(
    "The Dispatch process that answered does not offer this action yet, because a new release " +
      "of Dispatch is in progress. Try again in one minute."
  );
  expect(isNoRoute(error)).toBe(false);
  expect(stub.requests).toHaveLength(4);
  // Waits of 1 s, 2 s and 4 s, each jittered by a quarter either way.
  expect(wait.waits).toHaveLength(3);
  for (const [index, base] of [1_000, 2_000, 4_000].entries()) {
    expect(wait.waits[index]).toBeGreaterThanOrEqual(base * 0.75);
    expect(wait.waits[index]).toBeLessThanOrEqual(base * 1.25);
  }
});

test("a POST answered with no route is sent again with the same body", async () => {
  const stub = scriptedFetch([
    () => noRoute("POST", "/api/v1/issues"),
    () => Response.json({ key: "CORE-1" }, { status: 201 }),
  ]);
  const api = createApiClient(stub.fetch, undefined, recordedSleep().sleep);

  await api.createIssue({ project: "CORE", title: "Ship the widgets" }, { key: "write-1" });
  expect(stub.requests).toHaveLength(2);
  const [first, second] = stub.requests;
  expect(second?.path).toBe("/api/v1/issues");
  expect(second?.init?.method).toBe("POST");
  // The same intent, so the same key: no handler ran under the first attempt's no-route answer.
  expect(second?.init?.headers).toEqual(first?.init?.headers as Record<string, string>);
  expect(second?.init?.headers).toMatchObject({ "Content-Type": "application/json" });
  expect(JSON.parse(String(second?.init?.body))).toEqual({
    project: "CORE",
    title: "Ship the widgets",
  });
  expect(second?.init?.body).toBe(first?.init?.body);
});

test("a 404 NOT_FOUND for a resource the server does not know is not sent again", async () => {
  const stub = scriptedFetch([
    () => Response.json({ code: "NOT_FOUND", error: "issue not found" }, { status: 404 }),
  ]);
  const wait = recordedSleep();
  const api = createApiClient(stub.fetch, undefined, wait.sleep);

  await expect(api.getIssue("CORE-1")).rejects.toMatchObject({
    code: "NOT_FOUND",
    message: "issue not found",
    status: 404,
  });
  expect(stub.requests).toHaveLength(1);
  expect(wait.waits).toEqual([]);
});

test("a request aborted during a no-route wait rejects at once with the abort and is not sent again", async () => {
  const stub = scriptedFetch([
    () => noRoute("GET", "/api/github/rest/user"),
    () => Response.json({ login: "alice" }),
  ]);
  const controller = new AbortController();
  const reason = new Error("a newer read replaced this one");
  // The shared wait, with the caller's abort arriving once the wait has begun.
  const wait: Sleep = (ms, signal) => {
    const waiting = sleep(ms, signal);
    controller.abort(reason);
    return waiting;
  };
  // `githubRest` hands the caller's init to the one request path every method shares, so the
  // caller's signal is the one the wait is given.
  const api = createApiClient(stub.fetch, undefined, wait);

  await expect(api.githubRest("user", { signal: controller.signal })).rejects.toBe(reason);
  expect(stub.requests).toHaveLength(1);
});
