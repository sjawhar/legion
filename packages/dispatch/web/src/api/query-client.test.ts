import { expect, jest, spyOn, test } from "bun:test";
import { QueryObserver } from "@tanstack/react-query";

import { createApiClient, isRetryableQueryError } from "./client";
import { DispatchQueryClient, queryRetryDelay } from "./query-client";

/**
 * A server for one read whose every request is answered by the test: `answer()` lands the oldest
 * request still out with the value the server held when that request reached it.
 */
function heldServer(initial: string) {
  let value = initial;
  let inFlight = 0;
  let mostInFlight = 0;
  let requests = 0;
  const out: (() => void)[] = [];
  return {
    answer: () => out.shift()?.(),
    mostInFlight: () => mostInFlight,
    queryFn: () => {
      requests += 1;
      inFlight += 1;
      mostInFlight = Math.max(mostInFlight, inFlight);
      const read = value;
      const { promise, resolve } = Promise.withResolvers<string>();
      out.push(() => {
        inFlight -= 1;
        resolve(read);
      });
      return promise;
    },
    requests: () => requests,
    write: (next: string) => {
      value = next;
    },
  };
}

async function settle(): Promise<void> {
  for (let index = 0; index < 20; index += 1) {
    await Promise.resolve();
  }
}

test("a mutation's invalidation during a fetch ends on the answer after the write, with never two requests in flight", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["issues", "pinned"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    expect(server.requests()).toBe(1);

    // The write lands while the read is out, and the mutation invalidates the read - twice, as
    // two mutations settling together would.
    server.write("after");
    let invalidated = false;
    void queryClient.invalidateQueries({ queryKey: ["issues"] }).then(() => {
      invalidated = true;
    });
    void queryClient.invalidateQueries({ queryKey: ["issues", "pinned"] });
    await settle();
    // Nothing cancelled the read already out, and nothing went out beside it.
    expect(server.requests()).toBe(1);

    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["issues", "pinned"])).toBe("before");
    // One more request, sent once the first settled, which reads the write.
    expect(server.requests()).toBe(2);
    expect(invalidated).toBe(false);

    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["issues", "pinned"])).toBe("after");
    // The mutation's own wait ends on the answer composed after its write.
    expect(invalidated).toBe(true);
    expect(server.requests()).toBe(2);
    expect(server.mostInFlight()).toBe(1);
  } finally {
    unsubscribe();
  }
});

test("an invalidation of an idle read fetches it at once", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["projects"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    server.answer();
    await settle();

    server.write("after");
    const invalidation = queryClient.invalidateQueries({ queryKey: ["projects"] });
    await settle();
    expect(server.requests()).toBe(2);
    server.answer();
    await invalidation;
    expect(queryClient.getQueryData<string>(["projects"])).toBe("after");
  } finally {
    unsubscribe();
  }
});

/**
 * When each request of one read went out, in ms after the first, through main.tsx's retry options
 * and the real API client, with every response a 503 refusal carrying `headers`. The clock is
 * fake and moves in 10 ms steps until `ms` have passed.
 */
async function refusedReadStarts(headers: Record<string, string>, ms: number): Promise<number[]> {
  const starts: number[] = [];
  const client = createApiClient(async () => {
    starts.push(Date.now());
    return Response.json({ code: "OVERLOADED", error: "busy" }, { headers, status: 503 });
  });
  const queryClient = new DispatchQueryClient({
    defaultOptions: {
      queries: {
        retry: (failureCount, error) => failureCount < 2 && isRetryableQueryError(error),
        retryDelay: queryRetryDelay,
      },
    },
  });
  jest.useFakeTimers();
  try {
    void queryClient
      .fetchQuery({ queryFn: () => client.whoAmI(), queryKey: ["whoami"] })
      .catch(() => undefined);
    for (let elapsed = 0; elapsed < ms; elapsed += 10) {
      await settle();
      jest.advanceTimersByTime(10);
    }
    await settle();
  } finally {
    jest.useRealTimers();
    queryClient.clear();
  }
  return starts.map((start) => start - (starts[0] ?? start));
}

test("a read refused with Retry-After: 2 is retried no sooner than 2 s later and before 4 s", async () => {
  for (const draw of [0, 0.9999]) {
    const random = spyOn(Math, "random").mockReturnValue(draw);
    try {
      const starts = await refusedReadStarts({ "Retry-After": "2" }, 4_100);
      expect(starts[1]).toBeGreaterThanOrEqual(2_000);
      expect(starts[1]).toBeLessThan(4_000);
    } finally {
      random.mockRestore();
    }
  }
});

test("a read refused without Retry-After is retried at 500 ms", async () => {
  const starts = await refusedReadStarts({}, 700);
  expect(starts[1]).toBeGreaterThanOrEqual(500);
  expect(starts[1]).toBeLessThan(510);
});
