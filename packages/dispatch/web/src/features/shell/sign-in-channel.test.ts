import { expect, jest, test } from "bun:test";

import {
  OTHER_TABS_ANSWER_MS,
  SIGN_OUT_LEASE_MS,
  SIGN_OUT_RENEW_MS,
  SignInChannel,
  type SignInListener,
  type UnsentChange,
} from "./sign-in-channel";

/** A browser's tabs on one channel name: each channel it opens hears what every other posts, at
 *  once, as a `BroadcastChannel` hears its siblings - only sooner, so the fake clock decides every
 *  timing here. */
function browser() {
  const channels = new Set<EventTarget>();
  return (): BroadcastChannel => {
    const channel = new EventTarget();
    Object.assign(channel, {
      postMessage: (data: unknown) => {
        for (const other of [...channels]) {
          if (other !== channel) other.dispatchEvent(new MessageEvent("message", { data }));
        }
      },
    });
    channels.add(channel);
    return channel as BroadcastChannel;
  };
}

/** A browser's Web Locks, as `navigator.locks` grants them: one holder at a time per name, and
 *  every other request for that name queued in the order it was made, granted as the holder
 *  before it lets go. A request called off through its `signal` leaves the queue. `requested`
 *  records every name asked for, in order. */
function browserLocks(): { locks: LockManager; requested: string[] } {
  const queues = new Map<string, (() => void)[]>();
  const held = new Set<string>();
  const requested: string[] = [];
  const grant = (name: string) => {
    if (held.has(name)) return;
    const next = queues.get(name)?.shift();
    if (next === undefined) return;
    held.add(name);
    next();
  };
  const request = (name: string, first: unknown, second?: unknown) => {
    const options = (typeof first === "function" ? {} : (first ?? {})) as LockOptions;
    const granted = (typeof first === "function" ? first : second) as (lock: Lock) => unknown;
    const { promise, reject, resolve } = Promise.withResolvers<unknown>();
    const run = () => {
      const done = (end: () => void) => {
        held.delete(name);
        end();
        grant(name);
      };
      void Promise.resolve(granted({ mode: "exclusive", name })).then(
        (value) => done(() => resolve(value)),
        (error: unknown) =>
          done(() => reject(error instanceof Error ? error : new Error(String(error))))
      );
    };
    options.signal?.addEventListener("abort", () => {
      const queue = queues.get(name) ?? [];
      const at = queue.indexOf(run);
      if (at === -1) return;
      queue.splice(at, 1);
      reject(new Error("the lock request was called off"));
    });
    requested.push(name);
    queues.set(name, [...(queues.get(name) ?? []), run]);
    grant(name);
    return promise;
  };
  return {
    locks: {
      query: () => Promise.resolve({ held: [], pending: [] }),
      request,
    } as unknown as LockManager,
    requested,
  };
}

/** A tab on `open`'s browser: its channel, its page, and what its listener was told, each
 *  `[what, login]`; it answers a sign-out's question with `unsent`. `locks` is the browser's lock
 *  manager, as the page passes `navigator.locks`; with none, holds live on the renewal lease. */
function tab(
  open: () => BroadcastChannel,
  unsent: readonly UnsentChange[] = [],
  locks?: LockManager
) {
  const page = new EventTarget();
  const channel = open();
  const signIns = new SignInChannel(channel, page, locks);
  const heard: [string, string][] = [];
  const listener: SignInListener = {
    changed: () => {},
    holderGone: (login) => heard.push(["holderGone", login]),
    signedOut: (login) => heard.push(["signedOut", login]),
    signingOut: (login) => heard.push(["signingOut", login]),
    stillSignedIn: (login) => heard.push(["stillSignedIn", login]),
    unsent: () => unsent,
  };
  signIns.listen(listener);
  return { channel, heard, leave: () => page.dispatchEvent(new Event("pagehide")), signIns };
}

/** Runs `body` on the fake clock. */
async function onFakeClock(body: () => Promise<void> | void): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.useRealTimers();
  }
}

// W51, W58. Where the browser has no `navigator.locks` - a page outside a secure context - a
// sign-out's hold on the person's writes in another tab is a lease: the signing-out tab says so
// again every 2 s while its sign-out is out, and a holding tab lifts a hold it has not heard
// renewed for 6 s, so a tab that crashes or is killed with its sign-out out holds nothing for
// good. A tab that went that way said nothing of how its sign-out ended, so the hold ends as a
// holder gone (`holderGone`), not as a sign-out refused.
test("with no lock manager a sign-out's hold lapses 6 s after its tab last said so", async () => {
  await onFakeClock(() => {
    const open = browser();
    const signer = tab(open);
    const holder = tab(open);
    const said: unknown[] = [];
    open().addEventListener("message", (event) => said.push((event as MessageEvent).data));

    signer.signIns.signingOut("alice");
    expect(holder.heard).toEqual([["signingOut", "alice"]]);
    jest.advanceTimersByTime(SIGN_OUT_RENEW_MS * 9);
    // Said again every 2 s, it holds for as long as the sign-out is out, and is told once.
    expect(said.filter((data) => Reflect.get(Object(data), "signingOut") === "alice")).toHaveLength(
      10
    );
    expect(holder.heard).toEqual([["signingOut", "alice"]]);

    // The signing tab dies without a word, just after its last renewal: nothing it posts reaches
    // the others any more, and the hold lapses 6 s after that renewal.
    Object.assign(signer.channel, { postMessage: () => {} });
    jest.advanceTimersByTime(SIGN_OUT_LEASE_MS - 1);
    expect(holder.heard).toEqual([["signingOut", "alice"]]);
    jest.advanceTimersByTime(1);
    expect(holder.heard).toEqual([
      ["signingOut", "alice"],
      ["holderGone", "alice"],
    ]);
  });
});

// W58. Where the browser has locks, the hold is the signing-out tab's lock: it lives as long as
// that tab does, however late its timers run. A tab frozen or throttled with its sign-out out -
// Chrome wakes a tab hidden over five minutes about once a minute - says so again far too
// rarely for the lease, and must hold the person's writes all the same; only the browser taking
// its lock back, when the tab closes, crashes or is discarded, ends the hold.
test("with the browser's locks a hold outlives the lease, and ends when the lock is released", async () => {
  await onFakeClock(async () => {
    const open = browser();
    const { locks } = browserLocks();
    const signer = tab(open, [], locks);
    const holder = tab(open, [], locks);

    signer.signIns.signingOut("alice");
    await Promise.resolve();
    expect(holder.heard).toEqual([["signingOut", "alice"]]);

    // The tab is frozen with its sign-out out: it says nothing again for minutes, and its lock
    // keeps the hold anyway.
    Object.assign(signer.channel, { postMessage: () => {} });
    jest.advanceTimersByTime(SIGN_OUT_LEASE_MS * 20);
    await Promise.resolve();
    expect(holder.heard).toEqual([["signingOut", "alice"]]);

    // The browser takes the lock back, as it does when the tab goes: the hold ends, and what
    // became of the writes it held is the whoami read's to decide (`holderGone`).
    signer.leave();
    await Promise.resolve();
    await Promise.resolve();
    expect(holder.heard).toEqual([
      ["signingOut", "alice"],
      ["holderGone", "alice"],
    ]);
  });
});

// W58. Each sign-out's lock is its own tab's (`dispatch-sign-out:<login>:<tab>`), so two tabs
// signing the same person out in the same instant neither wait on nor hold each other's: the
// third tab waits on both, and the person's writes go out in no tab until both of those
// sign-outs are over. One tab dying with the other's still out must leave the hold exactly where
// it was.
test("two tabs signing the same person out hold a third tab's writes until both are gone", async () => {
  await onFakeClock(async () => {
    const open = browser();
    const { locks, requested } = browserLocks();
    const first = tab(open, [], locks);
    const second = tab(open, [], locks);
    const holder = tab(open, [], locks);

    first.signIns.signingOut("alice");
    second.signIns.signingOut("alice");
    await Promise.resolve();
    expect(holder.heard).toEqual([["signingOut", "alice"]]);
    // Four requests, two names: each tab's own lock, taken by it and waited on by the holder.
    expect(new Set(requested).size).toBe(2);
    for (const name of requested) expect(name.startsWith("dispatch-sign-out:alice:")).toBe(true);

    // The first tab dies without a word: the browser takes back its lock alone, and the second
    // tab's sign-out still holds the writes here.
    Object.assign(first.channel, { postMessage: () => {} });
    first.leave();
    jest.advanceTimersByTime(SIGN_OUT_LEASE_MS * 4);
    await Promise.resolve();
    await Promise.resolve();
    expect(holder.heard).toEqual([["signingOut", "alice"]]);

    // The second goes too: only now are the writes nobody's to hold.
    Object.assign(second.channel, { postMessage: () => {} });
    second.leave();
    await Promise.resolve();
    await Promise.resolve();
    expect(holder.heard).toEqual([
      ["signingOut", "alice"],
      ["holderGone", "alice"],
    ]);
  });
});

// W56. A tab that loads, or reloads, while another tab's sign-out is out hears of it on the
// channel before its app mounts and its listener attaches. The hold must reach that listener all
// the same: otherwise the tab resumes the very writes the person confirmed discarding.
test("a listener attaching while a sign-out is out is told to hold at once", async () => {
  await onFakeClock(() => {
    const open = browser();
    const signer = tab(open);
    const page = new EventTarget();
    const late = new SignInChannel(open(), page);

    signer.signIns.signingOut("alice");
    const heard: [string, string][] = [];
    late.listen({
      changed: () => {},
      holderGone: (login) => heard.push(["holderGone", login]),
      signedOut: (login) => heard.push(["signedOut", login]),
      signingOut: (login) => heard.push(["signingOut", login]),
      stillSignedIn: (login) => heard.push(["stillSignedIn", login]),
      unsent: () => [],
    });
    expect(heard).toEqual([["signingOut", "alice"]]);

    // The hold is the one it already had: it ends once, when that tab says how its sign-out
    // ended, and is not held twice by the renewals in between.
    jest.advanceTimersByTime(SIGN_OUT_RENEW_MS * 2);
    signer.signIns.stillSignedIn("alice");
    expect(heard).toEqual([
      ["signingOut", "alice"],
      ["stillSignedIn", "alice"],
    ]);
  });
});

test("a hold lifts at once when its tab goes, or says how its sign-out ended", async () => {
  await onFakeClock(() => {
    const open = browser();
    const holder = tab(open);

    // A tab that goes says nothing of how its sign-out ended: it does not know.
    const gone = tab(open);
    gone.signIns.signingOut("alice");
    gone.leave();
    expect(holder.heard).toEqual([
      ["signingOut", "alice"],
      ["holderGone", "alice"],
    ]);

    const refused = tab(open);
    refused.signIns.signingOut("alice");
    refused.signIns.stillSignedIn("alice");
    const landed = tab(open);
    landed.signIns.signingOut("alice");
    landed.signIns.signedOut("alice");
    expect(holder.heard.slice(2)).toEqual([
      ["signingOut", "alice"],
      ["stillSignedIn", "alice"],
      ["signingOut", "alice"],
      ["signedOut", "alice"],
    ]);
    // An ended sign-out renews nothing, so nothing is held again or lifted twice.
    jest.advanceTimersByTime(SIGN_OUT_LEASE_MS * 2);
    expect(holder.heard).toHaveLength(6);
  });
});

// Each hold is its tab's: one tab's sign-out ending lifts nothing another tab's still holds.
test("a hold is its tab's: the person's writes go on once no tab's holds them", async () => {
  await onFakeClock(() => {
    const open = browser();
    const holder = tab(open);
    const first = tab(open);
    const second = tab(open);

    first.signIns.signingOut("alice");
    second.signIns.signingOut("alice");
    first.signIns.stillSignedIn("alice");
    expect(holder.heard).toEqual([["signingOut", "alice"]]);
    second.leave();
    expect(holder.heard).toEqual([
      ["signingOut", "alice"],
      ["holderGone", "alice"],
    ]);
  });
});

// W53. A tab that does not answer a sign-out's question in time is taken for gone - closed
// without a word, it would otherwise hold every later sign-out for the whole wait and add "any
// unsent changes in your other Dispatch tabs" for good - until it next speaks: a late answer, or
// its hello as it comes back from the back/forward cache.
test("a tab that misses a question is dropped until it next speaks", async () => {
  await onFakeClock(async () => {
    const open = browser();
    const asker = tab(open);
    // A tab whose app has not mounted, or whose page is frozen: it answers nothing.
    const page = new EventTarget();
    new SignInChannel(open(), page);
    // A slow tab: it answers, but only once the question has been given up on.
    const slow = open();
    const asked: string[] = [];
    slow.addEventListener("message", (event) => {
      const ask: unknown = Reflect.get(Object((event as MessageEvent).data), "ask");
      if (typeof ask === "string") asked.push(ask);
    });
    slow.postMessage({ hello: true, tab: "slow" });

    const first = asker.signIns.unsentElsewhere("alice");
    jest.advanceTimersByTime(OTHER_TABS_ANSWER_MS);
    expect(await first).toEqual({ unanswered: true, unsent: [] });
    // Both taken for gone: the next question waits for no one.
    expect(await asker.signIns.unsentElsewhere("alice")).toEqual({
      unanswered: false,
      unsent: [],
    });

    // The slow tab's late answer is it speaking again, and so is the frozen tab's hello.
    slow.postMessage({ answer: asked[0], tab: "slow", unsent: [] });
    const second = asker.signIns.unsentElsewhere("alice");
    jest.advanceTimersByTime(OTHER_TABS_ANSWER_MS);
    expect(await second).toEqual({ unanswered: true, unsent: [] });
    page.dispatchEvent(Object.assign(new Event("pageshow"), { persisted: true }));
    const third = asker.signIns.unsentElsewhere("alice");
    jest.advanceTimersByTime(OTHER_TABS_ANSWER_MS);
    expect(await third).toEqual({ unanswered: true, unsent: [] });
    expect(await asker.signIns.unsentElsewhere("alice")).toEqual({
      unanswered: false,
      unsent: [],
    });
  });
});

// W53. Each tab answers with its writes' keys, so a tab duplicated from another - which copied its
// kept writes, and resumed them under the same keys - can be told apart from a tab with writes of
// its own.
test("every tab answers with the key and name of each write it would discard", async () => {
  await onFakeClock(async () => {
    const open = browser();
    const asker = tab(open);
    tab(open, [{ key: "k-1", label: "CORE-1 set to done" }]);
    tab(open, [{ key: "k-1", label: "CORE-1 set to done" }]);
    const elsewhere = await asker.signIns.unsentElsewhere("alice");
    expect(elsewhere).toEqual({
      unanswered: false,
      unsent: [
        { key: "k-1", label: "CORE-1 set to done" },
        { key: "k-1", label: "CORE-1 set to done" },
      ],
    });
  });
});
