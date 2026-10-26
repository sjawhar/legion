import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { keptTab, memoryFiles } from "../../__tests__/kept-sends-harness";
import {
  ApiError,
  api,
  NetworkError,
  RequestTimeoutError,
  type WriteBounds,
  type WriteRequest,
} from "../../api/client";
import { reachable, unreachable } from "../../api/reachability";
import {
  KEYED_SEND_ATTEMPT_MS,
  SEND_DEADLINE_MS,
  SendInterruptedError,
  SendStoppedError,
  WriteDiscardedError,
} from "./keyed-write";
import { KEPT_SEND_RESUME_MS } from "./write-storage";
import { defineWriteKind, plainWrite, type Write, Writes } from "./write-store";

/** A page's query client, signed in: the store stamps every write with this login at its press
 *  and resumes only this login's, as the app's does. */
function signedIn(login = "alice"): QueryClient {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

/** Lets every promise the store chained run as far as it goes without the clock moving. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
}

/** Runs `body` on the fake clock, which moves a millisecond before the real one comes back: a
 *  timer left at zero delay would otherwise hang a later test's `waitFor`. */
async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
}

/** Moves the clock `ms` on, letting each timer's continuation run as it fires. */
async function tick(ms: number): Promise<void> {
  for (let left = ms; left > 0; left -= 100) {
    jest.advanceTimersByTime(Math.min(100, left));
    await settle();
  }
}

/** A fetch that failed: what the client makes of a browser's `TypeError`. */
const dropped = (path = "/api/v1/issues/CORE-1") =>
  new NetworkError(path, new TypeError("Failed to fetch"));

/** One attempt that tells `sent` the request as `api.send` does on the real path, then answers
 *  with `answer()`. */
function attemptOf(
  request: WriteRequest,
  answer: () => Promise<unknown>
): (bounds: WriteBounds) => Promise<unknown> {
  return (bounds) => {
    bounds.sent?.(request);
    return answer();
  };
}

const patch: WriteRequest = {
  body: { status: "done" },
  method: "PATCH",
  path: "/api/v1/issues/CORE-1",
};

/** A status pick on CORE-1, as the indicator shows it, its attempts answered by `answer`. */
function statusWrite(
  writes: Writes,
  answer: () => Promise<unknown>,
  fields: { target?: string; label?: string } = {}
): Promise<unknown> {
  return writes.run({
    detail: null,
    kind: plainWrite,
    label: fields.label ?? "CORE-1 set to done",
    presentation: "indicator",
    target: fields.target,
    write: attemptOf(patch, answer),
  });
}

const unavailable = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });

/** A store of a page of `tab`, signed in as `login`. */
function storeOf(tab = keptTab(), login = "alice", files = memoryFiles()) {
  return {
    client: signedIn(login),
    files,
    tab,
    writes: new Writes(signedIn(login), tab.page(), files),
  };
}

/** The write `writes` holds now, at `index` in press order. */
const row = (writes: Writes, index = 0): Write | undefined => writes.rows()[index];

// One press is one key, and every attempt of that press carries it, so however many attempts
// reach Dispatch it is one write there. Only an attempt that ended short of Dispatch's verdict is
// made again: a connection that failed, an attempt gone silent, a 5xx, a 408, a 429, and the 409
// that says Dispatch is still running the first request under this key.
test("one key per press, reused by every attempt the store makes", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const answers: (() => Promise<unknown>)[] = [
      () => Promise.reject(dropped()),
      () => Promise.reject(new RequestTimeoutError(patch.path, KEYED_SEND_ATTEMPT_MS)),
      () => Promise.reject(unavailable),
      () => Promise.reject(new ApiError(408, { error: "request timeout" })),
      () => Promise.reject(new ApiError(429, { code: "RATE_LIMITED", error: "slow down" })),
      () => Promise.reject(new ApiError(409, { code: "KEY_IN_FLIGHT", error: "still running" })),
      async () => ({ key: "CORE-1" }),
    ];
    const keys: string[] = [];
    const saved = writes.run({
      detail: null,
      kind: plainWrite,
      label: "CORE-1 set to done",
      presentation: "indicator",
      write: (bounds) => {
        keys.push(bounds.key);
        bounds.sent?.(patch);
        return (answers.shift() ?? answers[0])?.() ?? Promise.resolve();
      },
    });
    await tick(40_000);
    expect(await saved).toEqual({ key: "CORE-1" });
    expect(keys).toHaveLength(7);
    expect(new Set(keys).size).toBe(1);
    expect(writes.rows()).toEqual([]);
  });
});

// Dispatch's verdict ends the write: a 4xx it answered is not made again, and the write stays for
// the reader with that refusal until Discard drops it. An error of the page's own - a bug - is
// refused at once too, never retried for good.
test("a 4xx, or an error of the page's own, refuses at once and waits for the reader", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let attempts = 0;
    const refused = statusWrite(writes, () => {
      attempts += 1;
      return Promise.reject(verdict);
    }).catch((error: unknown) => error);
    await settle();
    expect(await refused).toBe(verdict);
    const write = row(writes);
    expect(write).toMatchObject({
      label: "CORE-1 set to done",
      refusal: verdict,
      status: "refused",
    });
    await tick(60_000);
    expect(attempts).toBe(1);
    expect(writes.discard(write?.id ?? 0)).toBe(true);
    expect(writes.rows()).toEqual([]);

    const bug = new Error("bug");
    let buggy = 0;
    const failed = statusWrite(writes, () => {
      buggy += 1;
      return Promise.reject(bug);
    }).catch((error: unknown) => error);
    await tick(60_000);
    expect(await failed).toBe(bug);
    expect(buggy).toBe(1);
  });
});

// Past its deadline a write is late and the reader may stop it: no attempt follows the one in
// flight, it ends with `SendStoppedError`, and Retry asks again under the same key. A Retry after
// Dispatch's own refusal goes under a new key, since Dispatch replays the refusal it stored.
test("Stop trying ends a late write, and Retry keeps its key until Dispatch has refused", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const keys: string[] = [];
    let answer: () => Promise<unknown> = () => Promise.reject(unavailable);
    const stopped = writes
      .run({
        detail: null,
        kind: plainWrite,
        label: "CORE-1 set to done",
        presentation: "indicator",
        write: (bounds) => {
          keys.push(bounds.key);
          bounds.sent?.(patch);
          return answer();
        },
      })
      .catch((error: unknown) => error);
    await tick(SEND_DEADLINE_MS + 1_000);
    const late = row(writes);
    expect(late?.status).toBe("late");
    const before = keys.length;
    expect(writes.stop(late?.id ?? 0)).toBe(true);
    await tick(30 * 60_000);
    expect(await stopped).toBeInstanceOf(SendStoppedError);
    expect(keys).toHaveLength(before);
    expect(row(writes)?.status).toBe("refused");

    answer = () => Promise.reject(verdict);
    writes.retry(row(writes)?.id ?? 0);
    await settle();
    expect(keys.at(-1)).toBe(keys[0]);

    answer = async () => ({ key: "CORE-1" });
    writes.retry(row(writes)?.id ?? 0);
    await settle();
    expect(keys.at(-1)).not.toBe(keys[0]);
    expect(writes.rows()).toEqual([]);
  });
});

// A write is kept from its press, so a reload takes it up again under its key - which Dispatch
// answers with what the first did - and makes it once. One pressed longer ago than five minutes is
// not made again on its own: it comes back refused, for Retry under its key or Discard. A PUT and a
// DELETE are kept and resumed like any write (W29).
test("a write survives a reload and resumes once, PUT included, and an old one comes back refused", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    const put: WriteRequest = {
      body: { until: "2026-11-01T00:00:00Z" },
      method: "PUT",
      path: "/api/v1/asks/ask-1/snooze",
    };
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      const hang = () => Promise.withResolvers<never>().promise;
      void statusWrite(before, hang).catch(() => {});
      void before
        .run({
          detail: null,
          kind: plainWrite,
          label: "Ask snoozed",
          presentation: "indicator",
          write: attemptOf(put, hang),
        })
        .catch(() => {});
      await settle();
      const [patchKey, putKey] = before.rows().map((write) => write.key);
      expect(tab.kept(patchKey ?? "")).toMatchObject({
        kind: "write",
        login: "alice",
        state: { status: "sending" },
      });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      const resent: unknown[] = send.mock.calls.map((call) => [
        call[0],
        (call[1] as WriteBounds).key,
      ]);
      expect(resent).toEqual([
        [patch, patchKey],
        [put, putKey],
      ]);
      expect(after.rows()).toEqual([]);
      expect(tab.kept(patchKey ?? "")).toBeUndefined();

      const old = new Writes(signedIn(), tab.page(), memoryFiles());
      void statusWrite(old, hang).catch(() => {});
      await settle();
      const oldKey = row(old)?.key ?? "";
      const entry = tab.kept(oldKey) as { pressedAt: number };
      tab.storage.setItem(
        `dispatch.write:${oldKey}`,
        JSON.stringify({ ...entry, pressedAt: Date.now() - KEPT_SEND_RESUME_MS - 1 })
      );
      const later = new Writes(signedIn(), tab.page(), memoryFiles());
      later.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(2);
      const resumed = row(later);
      expect(resumed).toMatchObject({ label: "CORE-1 set to done", status: "refused" });
      expect(resumed?.status === "refused" ? resumed.refusal : undefined).toBeInstanceOf(
        SendInterruptedError
      );
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// W24. An upload's file is kept apart from its request, under the write's key, so a reload makes
// the upload again with the same file; it goes once the upload lands.
test("an upload's file survives a reload and the upload resumes with it", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = memoryFiles();
    const file = new File(["spec body"], "spec.md", { type: "text/markdown" });
    const form = new FormData();
    form.set("name", "spec.md");
    form.set("file", file);
    const upload: WriteRequest = {
      body: form,
      method: "POST",
      path: "/api/v1/issues/CORE-1/artifacts",
    };
    const send = spyOn(api, "send").mockResolvedValue({ artifact: {}, version: {} } as never);
    const before = new Writes(signedIn(), tab.page(), files);
    try {
      void before
        .run({
          detail: null,
          file,
          kind: plainWrite,
          label: "Upload of spec.md",
          presentation: "indicator",
          write: attemptOf(upload, () => Promise.withResolvers<never>().promise),
        })
        .catch(() => {});
      await settle();
      const key = row(before)?.key ?? "";
      expect(files.rows.get(key)).toBe(file);
      expect(tab.kept(key)).toMatchObject({
        request: {
          form: [
            { name: "name", value: "spec.md" },
            { file: true, name: "file" },
          ],
          method: "POST",
        },
      });

      const after = new Writes(signedIn(), tab.page(), files);
      after.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      const [resent, bounds] = send.mock.calls[0] ?? [];
      expect(bounds?.key).toBe(key);
      const body = resent?.body as FormData;
      expect(body.get("name")).toBe("spec.md");
      expect((body.get("file") as File).name).toBe("spec.md");
      expect(after.rows()).toEqual([]);
      expect(files.rows.has(key)).toBe(false);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// W26. A write belongs to the person who pressed it: another login's kept write is dropped unmade
// on load, and once someone else is signed in, every write of another login stops and goes - in
// memory and in storage - and its press is refused.
test("a write of another login is dropped, kept or in flight", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({} as never);
    const alice = new Writes(signedIn("alice"), tab.page(), memoryFiles());
    try {
      let attempts = 0;
      const pressed = statusWrite(alice, () => {
        attempts += 1;
        return Promise.reject(dropped());
      }).catch((error: unknown) => error);
      await settle();
      const key = row(alice)?.key ?? "";
      expect(row(alice)?.login).toBe("alice");

      const bob = new Writes(signedIn("bob"), tab.page(), memoryFiles());
      bob.resume("bob");
      await settle();
      expect(send).not.toHaveBeenCalled();
      expect(tab.kept(key)).toBeUndefined();

      alice.signedIn("bob");
      expect(await pressed).toBeInstanceOf(SendStoppedError);
      expect(alice.rows()).toEqual([]);
      const after = attempts;
      await tick(60_000);
      expect(attempts).toBe(after);
      expect(tab.kept(key)).toBeUndefined();
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

/** A composer's refused send, as far as the store is concerned: a write whose record is the
 *  reader's draft, kept with it. */
const draftWrite = defineWriteKind<{ readonly draft: string }>({
  kept: (value): value is { readonly draft: string } =>
    typeof value === "object" && value !== null && typeof Reflect.get(value, "draft") === "string",
  name: "test-draft",
});

/** Alice's writes when her session lapses: one being tried (an upload, its file kept), and one
 *  Dispatch refused, holding her draft. */
async function lapsedWrites() {
  const tab = keptTab();
  const files = memoryFiles();
  const writes = new Writes(signedIn("alice"), tab.page(), files);
  const file = new File(["spec body"], "spec.md", { type: "text/markdown" });
  let up = false;
  let attempts = 0;
  const out = writes
    .run({
      detail: null,
      file,
      kind: plainWrite,
      label: "Upload of spec.md",
      presentation: "indicator",
      write: attemptOf(patch, () => {
        attempts += 1;
        return up ? Promise.resolve({ ok: true }) : Promise.reject(dropped());
      }),
    })
    .catch((error: unknown) => error);
  void writes
    .run({
      detail: { draft: "My only copy of this" },
      kind: draftWrite,
      label: "Message",
      presentation: "composer",
      write: attemptOf(patch, () => Promise.reject(verdict)),
    })
    .catch(() => {});
  await settle();
  const [upload, draft] = writes.rows();
  writes.signedIn(undefined);
  await settle();
  return {
    attempts: () => attempts,
    draftKey: draft?.key ?? "",
    files,
    out,
    tab,
    up: () => {
      up = true;
    },
    uploadKey: upload?.key ?? "",
    writes,
  };
}

// W32. A session that lapses - whoami answers 401 - keeps the person's own writes: every one out
// stops trying and nothing more is sent, but each stays, in memory and in storage, a refused
// one's draft and an upload's file with it. Once the same person signs in again they go on: the
// one out is made again under its key, and the refusal is still there for Retry or Discard.
test("a lapsed session keeps its writes, and the same login's sign-in resumes them", async () => {
  await onFakeClock(async () => {
    const lapsed = await lapsedWrites();
    const { files, tab, writes } = lapsed;
    try {
      expect(writes.rows().map((write) => write.status)).toEqual(["queued", "refused"]);
      expect(tab.kept(lapsed.uploadKey)).toBeDefined();
      expect(tab.kept(lapsed.draftKey)).toMatchObject({
        detail: { draft: "My only copy of this" },
      });
      expect(files.rows.has(lapsed.uploadKey)).toBe(true);
      const stopped = lapsed.attempts();
      await tick(60_000);
      expect(lapsed.attempts()).toBe(stopped);

      lapsed.up();
      writes.signedIn("alice");
      await settle();
      expect(await lapsed.out).toEqual({ ok: true });
      expect(lapsed.attempts()).toBe(stopped + 1);
      expect(writes.rows()).toMatchObject([
        { detail: { draft: "My only copy of this" }, status: "refused" },
      ]);
      expect(tab.kept(lapsed.draftKey)).toMatchObject({
        detail: { draft: "My only copy of this" },
      });
      expect(files.rows.has(lapsed.uploadKey)).toBe(false);
    } finally {
      tab.close();
    }
  });
});

// W32. Only someone else's sign-in after the lapse drops them: then they go, in memory and in
// storage, files included, and the press is refused.
test("a lapsed session's writes go once another login signs in", async () => {
  await onFakeClock(async () => {
    const lapsed = await lapsedWrites();
    const { files, tab, writes } = lapsed;
    try {
      writes.signedIn("bob");
      await settle();
      expect(await lapsed.out).toBeInstanceOf(SendStoppedError);
      expect(writes.rows()).toEqual([]);
      expect(tab.kept(lapsed.uploadKey)).toBeUndefined();
      expect(tab.kept(lapsed.draftKey)).toBeUndefined();
      expect(files.rows.has(lapsed.uploadKey)).toBe(false);
    } finally {
      tab.close();
    }
  });
});

// W32. Dispatch answers a write whose session lapsed `401`, and nothing ran (the key is untouched):
// the write is not refused - it is asked again under its key - and the page reads who is signed in
// again, so a lapse it learns of this way holds every write until the sign-in that follows.
test("a write answered 401 is asked again under its key, and whoami is read again", async () => {
  await onFakeClock(async () => {
    const client = signedIn("alice");
    const invalidate = spyOn(client, "invalidateQueries");
    const writes = new Writes(client, keptTab().page(), memoryFiles());
    let answers = 0;
    const keys: string[] = [];
    const landed = writes.run({
      detail: null,
      kind: plainWrite,
      label: "CORE-1 set to done",
      presentation: "indicator",
      write: (bounds) => {
        keys.push(bounds.key);
        answers += 1;
        return answers === 1
          ? Promise.reject(new ApiError(401, { error: "unauthorized" }))
          : Promise.resolve({ key: "CORE-1" });
      },
    });
    await settle();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["whoami"] });
    expect(row(writes)?.status).toBe("sending");
    await tick(1_000);
    expect(await landed).toEqual({ key: "CORE-1" });
    expect(new Set(keys).size).toBe(1);
  });
});

// W32. A sign-out still out when the session lapses has done what it was for: it goes, rather than
// waiting to sign the person out again the moment they sign back in.
test("a sign-out still out is let go when the session lapses", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let attempts = 0;
    const signOut = writes
      .run({
        detail: null,
        endsWithSession: true,
        inline: true,
        kind: plainWrite,
        label: "Sign out",
        presentation: "indicator",
        write: () => {
          attempts += 1;
          return Promise.reject(dropped("/auth/logout"));
        },
      })
      .catch((error: unknown) => error);
    await settle();
    writes.signedIn(undefined);
    expect(await signOut).toBeInstanceOf(SendStoppedError);
    expect(writes.rows()).toEqual([]);
    writes.signedIn("alice");
    await tick(5_000);
    expect(attempts).toBe(1);
  });
});

// Dispatch refuses a write whose caller is not the person who pressed it (`Dispatch-Login`): the
// store drops every write of that login, and has the page read who is signed in again.
test("LOGIN_CHANGED drops every write of its login and reads whoami again", async () => {
  await onFakeClock(async () => {
    const client = signedIn("alice");
    const invalidate = spyOn(client, "invalidateQueries");
    const writes = new Writes(client, keptTab().page(), memoryFiles());
    let sentLogin: string | undefined;
    void statusWrite(writes, () => Promise.withResolvers<never>().promise, {
      target: "issue:CORE-2",
    }).catch(() => {});
    const refused = writes
      .run({
        detail: null,
        kind: plainWrite,
        label: "CORE-1 set to done",
        presentation: "indicator",
        write: (bounds) => {
          sentLogin = bounds.login;
          return Promise.reject(
            new ApiError(409, { code: "LOGIN_CHANGED", error: "signed in as bob" })
          );
        },
      })
      .catch((error: unknown) => error);
    await settle();
    expect(sentLogin).toBe("alice");
    expect(await refused).toBeInstanceOf(ApiError);
    expect(writes.rows()).toEqual([]);
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["whoami"] });
  });
});

// W25. Writes to one target go out in press order: a later one waits while an earlier one is still
// being tried, a refused one no longer holds the line, and its Retry joins at the back.
test("writes to one target go in press order, and a refused one does not hold the line", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const made: string[] = [];
    const first = Promise.withResolvers<unknown>();
    const press = (label: string, answer: () => Promise<unknown>) =>
      statusWrite(
        writes,
        () => {
          made.push(label);
          return answer();
        },
        { label, target: "session:planner" }
      ).catch(() => {});
    void press("first", () => first.promise);
    void press("second", async () => ({}));
    await settle();
    expect(made).toEqual(["first"]);
    expect(writes.rows().map((write) => write.status)).toEqual(["sending", "queued"]);

    first.reject(verdict);
    await settle();
    expect(made).toEqual(["first", "second"]);
    expect(writes.rows().map((write) => [write.label, write.status])).toEqual([
      ["first", "refused"],
    ]);

    void press("third", () => Promise.withResolvers<never>().promise);
    await settle();
    writes.retry(row(writes)?.id ?? 0);
    await settle();
    // The retry joined behind the third, still out.
    expect(made).toEqual(["first", "second", "third"]);
    expect(writes.rows().map((write) => [write.label, write.status])).toEqual([
      ["first", "queued"],
      ["third", "sending"],
    ]);
  });
});

// A write the reader never sees - a read mark - is tried and kept like any other, its refusal is
// never shown, and it is let go once five minutes pass without Dispatch confirming it.
test("a quiet write is never shown refused and is let go after five minutes", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    const quiet = (answer: () => Promise<unknown>) =>
      writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "Read mark",
          presentation: "quiet",
          write: attemptOf(patch, answer),
        })
        .catch((error: unknown) => error);
    const refused = quiet(() => Promise.reject(verdict));
    await settle();
    expect(await refused).toBe(verdict);
    expect(writes.rows()).toEqual([]);

    const unconfirmed = quiet(() => Promise.reject(dropped()));
    await settle();
    const key = row(writes)?.key ?? "";
    expect(tab.kept(key)).toMatchObject({ presentation: "quiet" });
    await tick(KEPT_SEND_RESUME_MS + 1_000);
    expect(await unconfirmed).toBeInstanceOf(SendStoppedError);
    expect(writes.rows()).toEqual([]);
    expect(tab.kept(key)).toBeUndefined();
  });
});

// W31. A write waiting out its backoff is asked again as soon as the network comes back: after a
// 40 s outage it lands within 2 s of the browser's `online`, rather than after the rest of a 30 s
// pause. A `409 KEY_IN_FLIGHT` waits the `Retry-After` Dispatch named.
test("a write lands within 2 s of the network returning, and KEY_IN_FLIGHT honours Retry-After", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let up = false;
    const landed = statusWrite(writes, () =>
      up ? Promise.resolve({ key: "CORE-1" }) : Promise.reject(dropped())
    );
    let done = false;
    void landed.then(() => {
      done = true;
    });
    await tick(40_000);
    expect(done).toBe(false);
    up = true;
    window.dispatchEvent(new Event("online"));
    await tick(2_000);
    expect(done).toBe(true);

    const times: number[] = [];
    const start = Date.now();
    let answers = 0;
    void statusWrite(writes, () => {
      times.push(Date.now() - start);
      answers += 1;
      return answers === 1
        ? Promise.reject(new ApiError(409, { code: "KEY_IN_FLIGHT", error: "running" }, 3_000))
        : Promise.resolve({});
    });
    await tick(2_500);
    // An answer elsewhere does not cut Dispatch's own Retry-After short.
    window.dispatchEvent(new Event("online"));
    await tick(1_000);
    expect(times).toHaveLength(2);
    expect(times[1]).toBeGreaterThanOrEqual(3_000);
    expect(times[1]).toBeLessThan(3_200);
  });
});

// A write Dispatch never answers is asked again after 1 s, 2 s, 4 s, 8 s and 16 s, and never more
// than 30 s apart after that, however long the outage lasts.
test("the backoff doubles from 1 s and is capped at 30 s", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const times: number[] = [];
    const start = Date.now();
    void statusWrite(writes, () => {
      times.push(Date.now() - start);
      return Promise.reject(dropped());
    }).catch(() => {});
    await tick(1_000 + 2_000 + 4_000 + 8_000 + 16_000 + 30_000 + 30_000 + 500);
    const gaps = times.slice(1).map((at, index) => at - (times[index] ?? 0));
    // Each gap is read at the 100 ms the clock moves by.
    expect(gaps.map((gap) => Math.round(gap / 1_000))).toEqual([1, 2, 4, 8, 16, 30, 30]);
  });
});

// A DELETE whose answer was lost and that Dispatch answers 404 on the next attempt was made: the
// thing it removes is gone either way, so it lands rather than being refused.
test("a resent DELETE answered 404 lands", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const remove: WriteRequest = {
      method: "DELETE",
      path: "/api/v1/projects/CORE/architecture-sources/legion/arch",
    };
    let attempts = 0;
    const landed = writes.run({
      detail: null,
      kind: plainWrite,
      label: "Source removed",
      presentation: "indicator",
      write: attemptOf(remove, () => {
        attempts += 1;
        return Promise.reject(
          attempts === 1 ? dropped() : new ApiError(404, { code: "SOURCE_NOT_FOUND" })
        );
      }),
    });
    await tick(1_500);
    expect(await landed).toBeNull();
    expect(writes.rows()).toEqual([]);
  });
});

// The write landed and its answer was lost: a repeat under the key is `409 KEY_COMMITTED`. That is
// a landing with no answer: the press resolves `null`, nothing stays or is kept, and every read is
// refreshed.
test("409 KEY_COMMITTED is a landing: null, nothing kept, and every read refreshed", async () => {
  await onFakeClock(async () => {
    const { tab } = storeOf();
    const client = signedIn();
    const invalidate = spyOn(client, "invalidateQueries");
    const writes = new Writes(client, tab.page(), memoryFiles());
    let attempts = 0;
    const landed = statusWrite(writes, () => {
      attempts += 1;
      return Promise.reject(
        attempts === 1 ? unavailable : new ApiError(409, { code: "KEY_COMMITTED", error: "made" })
      );
    });
    await settle();
    const key = row(writes)?.key ?? "";
    await tick(1_000);
    expect(await landed).toBeNull();
    expect(writes.rows()).toEqual([]);
    expect(tab.kept(key)).toBeUndefined();
    expect(invalidate).toHaveBeenCalledWith();
    await tick(60_000);
    expect(attempts).toBe(2);
  });
});

// The reader was stopping a write when the page reloaded, so its last attempt went with the page.
// The reload asks once more under its key and makes no attempt after that one.
test("a write the reader was stopping resumes with exactly one attempt", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockRejectedValue(unavailable);
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      void statusWrite(before, () => Promise.reject(unavailable)).catch(() => {});
      await tick(SEND_DEADLINE_MS + 1_000);
      expect(before.stop(row(before)?.id ?? 0)).toBe(true);
      const key = row(before)?.key ?? "";
      expect(tab.kept(key)).toMatchObject({ state: { status: "late", stopping: true } });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await tick(30 * 60_000);
      expect(send).toHaveBeenCalledTimes(1);
      expect(send.mock.calls[0]?.[1]?.key).toBe(key);
      const resumed = row(after);
      expect(resumed?.status).toBe("refused");
      expect(resumed?.status === "refused" && resumed.refusal).toBeInstanceOf(SendStoppedError);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// (a) Every write is kept from its press, one waiting its turn behind another on its line too: it
// is kept with the request it will make and its place in the line, so a reload during an outage
// makes both again, each once, in press order.
test("two writes on one line survive a reload and land once each, in press order", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    const landed: unknown[] = [];
    const send = spyOn(api, "send").mockImplementation(async (request) => {
      landed.push(request.body);
      return {} as never;
    });
    try {
      for (const status of ["backlog", "done"]) {
        const request: WriteRequest = { ...patch, body: { status } };
        void before
          .run({
            detail: null,
            kind: plainWrite,
            label: `CORE-1 set to ${status}`,
            presentation: "indicator",
            target: "issue:CORE-1",
            write: attemptOf(request, () => Promise.reject(dropped())),
          })
          .catch(() => {});
      }
      await settle();
      expect(before.rows().map((write) => write.status)).toEqual(["sending", "queued"]);
      expect(tab.kept(row(before, 1)?.key ?? "")).toMatchObject({
        request: { body: { status: "done" }, method: "PATCH" },
        sent: false,
        state: { status: "queued" },
      });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      expect(landed).toEqual([{ status: "backlog" }, { status: "done" }]);
      expect(after.rows()).toEqual([]);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// W37. A write waiting its turn that was never sent is the reader's to discard - its press is
// refused, and nothing of it goes out - while one that is out is not.
test("a write waiting its turn and never sent can be discarded; one out cannot", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let secondMade = 0;
    void statusWrite(writes, () => Promise.withResolvers<never>().promise, {
      target: "issue:CORE-1",
    }).catch(() => {});
    const waiting = statusWrite(
      writes,
      async () => {
        secondMade += 1;
        return {};
      },
      { label: "second", target: "issue:CORE-1" }
    ).catch((error: unknown) => error);
    await settle();
    expect(row(writes, 1)).toMatchObject({ sent: false, status: "queued" });
    expect(writes.discard(row(writes, 0)?.id ?? 0)).toBe(false);
    expect(writes.discard(row(writes, 1)?.id ?? 0)).toBe(true);
    expect(await waiting).toBeInstanceOf(WriteDiscardedError);
    expect(writes.rows().map((write) => write.status)).toEqual(["sending"]);
    expect(secondMade).toBe(0);
  });
});

// W39. Only Dispatch becoming reachable again resets a failing write's backoff. A route that keeps
// answering 503 while every other request the page makes succeeds backs off 1 s, 2 s, 4 s … to its
// 30 s cap; once a request has failed to reach Dispatch, the next answer starts it over.
test("a route that keeps answering 503 backs off to its cap while other requests succeed", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const times: number[] = [];
    const start = Date.now();
    void statusWrite(writes, () => {
      times.push(Date.now() - start);
      return Promise.reject(unavailable);
    }).catch(() => {});
    for (let elapsed = 0; elapsed < 91_500; elapsed += 500) {
      // Another route's 2xx, while Dispatch was reachable all along.
      reachable("answer");
      await tick(500);
    }
    const gaps = times.slice(1).map((at, index) => at - (times[index] ?? 0));
    expect(gaps.map((gap) => Math.round(gap / 1_000))).toEqual([1, 2, 4, 8, 16, 30, 30]);

    unreachable();
    reachable("answer");
    const made = times.length;
    await tick(2_200);
    expect(times.length).toBe(made + 2);
  });
});
