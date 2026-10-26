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

/** A tab on `open`'s browser: its channel, its page, and what its listener was told, each
 *  `[what, login]`; it answers a sign-out's question with `unsent`. */
function tab(open: () => BroadcastChannel, unsent: readonly UnsentChange[] = []) {
  const page = new EventTarget();
  const channel = open();
  const signIns = new SignInChannel(channel, page);
  const heard: [string, string][] = [];
  const listener: SignInListener = {
    changed: () => {},
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

// W51. A sign-out's hold on the person's writes in another tab is a lease: the signing-out tab
// says so again every 2 s while its sign-out is out, and a holding tab lifts a hold it has not
// heard renewed for 6 s, so a tab that crashes, is killed or is frozen with its sign-out out holds
// nothing for good.
test("a sign-out's hold lapses 6 s after its tab last said so, and renewing keeps it", async () => {
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
      ["stillSignedIn", "alice"],
    ]);
  });
});

test("a hold lifts at once when its tab goes, or says how its sign-out ended", async () => {
  await onFakeClock(() => {
    const open = browser();
    const holder = tab(open);

    const gone = tab(open);
    gone.signIns.signingOut("alice");
    gone.leave();
    expect(holder.heard).toEqual([
      ["signingOut", "alice"],
      ["stillSignedIn", "alice"],
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
      ["stillSignedIn", "alice"],
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
