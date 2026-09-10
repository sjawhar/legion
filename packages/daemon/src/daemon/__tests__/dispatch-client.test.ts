import { describe, expect, it } from "bun:test";
import { createDispatchClient, DispatchHttpError } from "../dispatch-client";

function fakeFetch(handler: (url: string, init: RequestInit) => Response): typeof fetch {
  return (async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
    return handler(String(input), init ?? {});
  }) as typeof fetch;
}

describe("createDispatchClient", () => {
  it("sends setStatus as a PATCH with the bearer header and the daemon's session actor body", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const client = createDispatchClient({
      baseUrl: "http://127.0.0.1:8766",
      token: "test-token",
      project: "LEGION",
      fetch: fakeFetch((url, init) => {
        requests.push({ url, init });
        return new Response("{}", { status: 200 });
      }),
    });

    await client.setStatus("LEGION-7", "in_progress");

    expect(requests).toHaveLength(1);
    const request = requests[0];
    if (!request) throw new Error("expected a request");
    expect(request.url).toBe("http://127.0.0.1:8766/api/v1/issues/LEGION-7");
    expect(request.init.method).toBe("PATCH");
    const headers = request.init.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-token");
    expect(headers["Content-Type"]).toBe("application/json");
    expect(JSON.parse(request.init.body as string)).toEqual({
      status: "in_progress",
      actor: {
        kind: "session",
        id: "legion-daemon:LEGION",
        origin: { session_title: "Legion daemon · LEGION" },
      },
    });
    // No top-level `origin` sibling: Dispatch's PATCH decoder rejects unknown top-level fields.
    expect(Object.keys(JSON.parse(request.init.body as string))).toEqual(["status", "actor"]);
  });

  it("throws DispatchHttpError with the status and the server's error text on a non-2xx response", async () => {
    const client = createDispatchClient({
      baseUrl: "http://127.0.0.1:8766",
      token: "test-token",
      project: "LEGION",
      fetch: fakeFetch(
        () => new Response(JSON.stringify({ error: "unknown status transition" }), { status: 400 })
      ),
    });

    await expect(client.setStatus("LEGION-7", "done")).rejects.toMatchObject({
      name: "DispatchHttpError",
      status: 400,
      message: "unknown status transition",
    });
    await expect(client.setStatus("LEGION-7", "done")).rejects.toBeInstanceOf(DispatchHttpError);
  });

  it("falls back to the response status text when a non-2xx body has no error field", async () => {
    const client = createDispatchClient({
      baseUrl: "http://127.0.0.1:8766",
      token: "test-token",
      project: "LEGION",
      fetch: fakeFetch(() => new Response("", { status: 500, statusText: "Internal Error" })),
    });

    await expect(client.setStatus("LEGION-7", "done")).rejects.toMatchObject({
      status: 500,
      message: "Internal Error",
    });
  });

  it("sends listIssues and getIssue as GET requests without a body", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const client = createDispatchClient({
      baseUrl: "http://127.0.0.1:8766/",
      token: "test-token",
      project: "LEGION",
      fetch: fakeFetch((url, init) => {
        requests.push({ url, init });
        return new Response("[]", { status: 200 });
      }),
    });

    await client.listIssues("LEGION");
    await client.getIssue("LEGION-7");

    expect(requests[0]?.url).toBe("http://127.0.0.1:8766/api/v1/issues?project=LEGION");
    expect(requests[0]?.init.method).toBe("GET");
    expect(requests[0]?.init.body).toBeUndefined();
    expect(requests[1]?.url).toBe("http://127.0.0.1:8766/api/v1/issues/LEGION-7");
    expect(requests[1]?.init.method).toBe("GET");
  });
});
