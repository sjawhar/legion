// packages/broker-client/src/__tests__/renewal.test.ts
//
// What the client does between trades: it holds a token while that token is live, trades again
// before it lapses with nothing of the caller's restarting, keeps writing through a broker that
// is merely down, and stops with a plain error rather than handing out no identity.

import { expect, test } from "bun:test";
import { newDpopKey } from "../dpop";
import type { SubjectToken, TokenSource } from "../source";
import { BrokerRefusedError, BrokerTokenClient } from "../token-client";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, stubBroker } from "./stub";

/** A source that answers a token of its own at each read, so a renewal is visibly a fresh one. */
function countingSource(): TokenSource & { reads: number } {
  const source = {
    reads: 0,
    read: async (): Promise<SubjectToken> => {
      source.reads += 1;
      return { token: `subject-${source.reads}`, tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES };
    },
    describe: () => "a scripted identity",
  };
  return source;
}

/** A client of `broker` whose clock this test moves. */
async function clientAt(
  url: string,
  clock: { now: number },
  source: TokenSource,
  renewBeforeMs = 60_000
): Promise<BrokerTokenClient> {
  return new BrokerTokenClient({
    brokerUrl: url,
    key: await newDpopKey(),
    subject: source,
    renewBeforeMs,
    now: () => clock.now,
  });
}

test("the token in hand is reused until the renewal window, then traded again", async () => {
  const broker = stubBroker((_trade, count) => ({ body: accepted(`token-${count}`, 600) }));
  const clock = { now: Date.now() };
  const source = countingSource();
  const client = await clientAt(broker.url, clock, source);
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    // Well inside the token's life: the same token, and no second trade.
    clock.now += 500_000;
    expect((await client.token()).accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(1);
    // Inside the renewal window: a fresh subject token traded for a fresh access token, with
    // nothing of the caller's restarted.
    clock.now += 60_000;
    expect((await client.token()).accessToken).toBe("token-2");
    expect(broker.trades.length).toBe(2);
    expect(broker.trades[1]?.form.get("subject_token")).toBe("subject-2");
    expect(source.reads).toBe(2);
  } finally {
    broker.stop();
  }
});

test("two callers that need a token at once make one trade", async () => {
  const broker = stubBroker((_trade, count) => ({ body: accepted(`token-${count}`, 600) }));
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    const [first, second] = await Promise.all([client.token(), client.token()]);
    expect(first.accessToken).toBe("token-1");
    expect(second.accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(1);
  } finally {
    broker.stop();
  }
});

test("a broker that does not answer leaves the token in hand working until it lapses", async () => {
  const broker = stubBroker((_trade, count) =>
    count === 1 ? { body: accepted("token-1", 600) } : { status: 503, body: { error: "down" } }
  );
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    // In the renewal window with the broker down: the token in hand is still live, so the caller
    // keeps writing with it.
    clock.now += 560_000;
    expect((await client.token()).accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(2);
    // Past its expiry there is nothing live left, so the caller is told rather than writing with
    // no identity.
    clock.now += 60_000;
    expect(client.token()).rejects.toThrow("503");
  } finally {
    broker.stop();
  }
});

test("a refusal of the identity stops at once, whatever is in hand", async () => {
  const broker = stubBroker((_trade, count) =>
    count === 1
      ? { body: accepted("token-1", 600) }
      : {
          status: 400,
          body: { error: "invalid_grant", error_description: "this sign-in was revoked" },
        }
  );
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    clock.now += 560_000;
    const refusal = await client.token().then(
      () => null,
      (error: unknown) => error
    );
    expect(refusal).toBeInstanceOf(BrokerRefusedError);
    expect((refusal as Error).message).toContain("this sign-in was revoked");
    // Nothing is held after a refusal: the next call asks again rather than answering a token the
    // broker has disowned.
    expect(client.token()).rejects.toThrow("this sign-in was revoked");
  } finally {
    broker.stop();
  }
});

test("the broker's rate limit leaves the token in hand working until it lapses", async () => {
  // The broker's token endpoint spends a bucket per source address, and a bucket refills: a 429
  // is the one refusal a later trade may be answered differently, so a client that holds a live
  // token keeps writing with it rather than failing every call through the renewal window.
  const broker = stubBroker((_trade, count) =>
    count === 1
      ? { body: accepted("token-1", 600) }
      : {
          status: 429,
          body: { error: "invalid_request", error_description: "too many token requests" },
        }
  );
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    clock.now += 560_000;
    expect((await client.token()).accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(2);
    // Past its expiry there is nothing live left, so the caller is told rather than writing with
    // no identity, and the refusal says a later trade may be answered differently.
    clock.now += 60_000;
    const refusal = await client.token().then(
      () => null,
      (error: unknown) => error
    );
    expect(refusal).toBeInstanceOf(BrokerRefusedError);
    expect((refusal as BrokerRefusedError).permanent).toBe(false);
    expect((refusal as Error).message).toContain("too many token requests");
  } finally {
    broker.stop();
  }
});

test("a broker asking for a nonce is answered with one in the next proof", async () => {
  const broker = stubBroker((trade, count) => {
    if (count === 1) {
      return { status: 400, body: { error: "use_dpop_nonce" }, nonce: "nonce-from-the-broker" };
    }
    return trade.proofClaims.nonce === "nonce-from-the-broker"
      ? { body: accepted("token-after-the-nonce", 600) }
      : { status: 400, body: { error: "use_dpop_nonce" }, nonce: "nonce-from-the-broker" };
  });
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-after-the-nonce");
    expect(broker.trades.length).toBe(2);
    expect(broker.trades[0]?.proofClaims.nonce).toBeUndefined();
  } finally {
    broker.stop();
  }
});
