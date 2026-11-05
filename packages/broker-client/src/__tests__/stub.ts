// packages/broker-client/src/__tests__/stub.ts
//
// The stub broker the renewal and environment suites trade against. The wire suite keeps a
// recorder of its own, since what it asserts is the raw shape of a request rather than the
// behaviour around it.

/** One trade as the stub saw it. */
export interface StubTrade {
  readonly form: URLSearchParams;
  readonly proofHeader: Record<string, unknown>;
  readonly proofClaims: Record<string, unknown>;
}

/** What the stub answers one trade with. */
export interface StubAnswer {
  readonly status?: number;
  readonly body: Record<string, unknown>;
  /** A `DPoP-Nonce` to send with the answer, which the next proof must carry. */
  readonly nonce?: string;
}

/** A broker that answers `answer` to each trade in turn, keeping every trade it was sent. */
export function stubBroker(answer: (trade: StubTrade, count: number) => StubAnswer): {
  readonly url: string;
  readonly trades: StubTrade[];
  stop(): void;
} {
  const trades: StubTrade[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const parts = (request.headers.get("DPoP") ?? "").split(".");
      const read = (at: number): Record<string, unknown> =>
        parts.length < 3
          ? {}
          : (JSON.parse(Buffer.from(parts[at] as string, "base64url").toString("utf8")) as Record<
              string,
              unknown
            >);
      const trade: StubTrade = {
        form: new URLSearchParams(await request.text()),
        proofHeader: read(0),
        proofClaims: read(1),
      };
      trades.push(trade);
      const given = answer(trade, trades.length);
      return Response.json(given.body, {
        status: given.status ?? 200,
        ...(given.nonce === undefined ? {} : { headers: { "DPoP-Nonce": given.nonce } }),
      });
    },
  });
  return { url: `http://127.0.0.1:${server.port}`, trades, stop: () => server.stop(true) };
}

/** An accepted answer carrying `accessToken`, good for `seconds`. */
export function accepted(accessToken: string, seconds: number): Record<string, unknown> {
  return {
    access_token: accessToken,
    issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
    token_type: "DPoP",
    expires_in: seconds,
    credential_id: "cred-1",
  };
}
