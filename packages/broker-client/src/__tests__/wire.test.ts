// packages/broker-client/src/__tests__/wire.test.ts
//
// The TypeScript half of the one wire contract: this replays every exchange in
// packages/envoy/internal/brokerclient/testdata/wire.json — that file itself, read by relative
// path, never a copy — against a stub broker and holds this client to it, as the Go suite's
// wire_test.go replays the same file against the Go one. The assertions are set equality, never
// containment — the corpus is the whole of what a request may carry and the whole of what a
// client may read out of an answer — so a field either client starts sending or reading without
// an entry in that file fails here, and a field added to that file fails the client that does not
// send it yet.

import { afterAll, expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { newDpopKey } from "../dpop";
import { metadataIdentity } from "../metadata-identity";
import { projectedToken } from "../projected-token";
import type { SubjectToken, TokenSource } from "../source";
import { BrokerRefusedError, type BrokerToken, BrokerTokenClient } from "../token-client";
import { FORM_LAUNCHER_CREDENTIAL, FORM_LAUNCHER_CREDENTIAL_DECLINED, TOKEN_PATH } from "../wire";

const makeDirectory = temporaryDirectories("broker-client");

/** The one file both clients read. It lives in the Go package's testdata because the Go suite is
 *  the half that existed first; this suite reads it by relative path. */
const CORPUS_PATH = new URL(
  "../../../envoy/internal/brokerclient/testdata/wire.json",
  import.meta.url
);

/**
 * Each member of {@link BrokerToken} under the name the corpus gives it in `expect.token`. The
 * type makes a member added without an entry a typecheck failure, and
 * `the corpus names every field a client reads` makes one named here but in no corpus entry a
 * test failure, which is how "the corpus is the whole of what a client reads" stays true.
 */
const TOKEN_FIELD_NAMES = {
  accessToken: "access_token",
  tokenType: "token_type",
  expiresAt: "expires_in",
  credentialId: "credential_id",
  credentialExpiresAt: "credential_expires_at",
} as const satisfies Record<keyof BrokerToken, string>;

interface CorpusToken {
  readonly kind: string;
  readonly token: string;
  readonly token_type: string;
}

interface CorpusExchange {
  readonly name: string;
  readonly subject: CorpusToken;
  /** The machine a person signed in on; absent from every trade a machine makes for itself. */
  readonly actor?: CorpusToken;
  readonly session: string;
  readonly form: Record<string, string>;
  readonly proof: {
    readonly header: Record<string, unknown>;
    readonly claims: Record<string, unknown>;
  };
  readonly answer: { readonly status: number; readonly body: Record<string, unknown> };
  readonly expect: {
    readonly outcome: string;
    readonly permanent?: boolean;
    readonly token?: Record<string, unknown>;
    readonly error_contains?: string;
  };
}

interface Corpus {
  readonly version: number;
  readonly endpoint: {
    readonly method: string;
    readonly path: string;
    readonly content_type: string;
  };
  readonly exchanges: readonly CorpusExchange[];
}

const corpus = (await Bun.file(CORPUS_PATH).json()) as Corpus;
if (corpus.exchanges.length === 0) throw new Error("the wire corpus names no exchange at all");

/** One trade as the stub broker saw it. */
interface StubRequest {
  readonly method: string;
  readonly path: string;
  readonly contentType: string;
  readonly form: URLSearchParams;
  readonly proofHeader: Record<string, unknown>;
  readonly proofClaims: Record<string, unknown>;
}

const servers: { stop: () => void }[] = [];
afterAll(() => {
  for (const server of servers) server.stop();
});

/** One part of a compact JWS as JSON. */
function part(compact: string, at: number): Record<string, unknown> {
  const encoded = compact.split(".")[at];
  if (encoded === undefined) throw new Error(`the DPoP proof has no part ${at}: ${compact}`);
  return JSON.parse(Buffer.from(encoded, "base64url").toString("utf8")) as Record<string, unknown>;
}

/** A broker that answers `status` and `body` to every trade, and keeps what it was sent. */
function stubBroker(status: number, body: Record<string, unknown>) {
  const requests: StubRequest[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const proof = request.headers.get("DPoP") ?? "";
      requests.push({
        method: request.method,
        path: new URL(request.url).pathname,
        contentType: request.headers.get("content-type") ?? "",
        form: new URLSearchParams(await request.text()),
        proofHeader: proof === "" ? {} : part(proof, 0),
        proofClaims: proof === "" ? {} : part(proof, 1),
      });
      return Response.json(body, { status });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, requests };
}

/**
 * A Compute Engine metadata server answering `token`, and the identity that reads it. It refuses
 * a request that does not ask the way the contract says: the identity path, the Metadata-Flavor
 * header, `format=full` and the audience.
 */
function metadataSource(token: string): TokenSource {
  const audience = "example-machine-audience";
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const url = new URL(request.url);
      if (url.pathname !== "/computeMetadata/v1/instance/service-accounts/default/identity") {
        return new Response("not the identity path", { status: 404 });
      }
      if (request.headers.get("Metadata-Flavor") !== "Google") {
        return new Response("no Metadata-Flavor header", { status: 403 });
      }
      if (url.searchParams.get("format") !== "full") {
        return new Response("the identity must be asked for with format=full", { status: 400 });
      }
      if (url.searchParams.get("audience") !== audience) {
        return new Response("another audience", { status: 400 });
      }
      return new Response(token);
    },
  });
  servers.push(server);
  return metadataIdentity(audience, {
    GCE_METADATA_HOST: `127.0.0.1:${server.port}`,
  } as NodeJS.ProcessEnv);
}

/**
 * The identity an entry's subject names, built from the real reader wherever one exists: a
 * projected token is read from a file this test writes, and a machine's Google identity token
 * from a metadata server this test serves. A person's sign-in is scripted here, its own reader
 * being the device grant, which `agent-secrets login` runs through the Go client.
 */
async function corpusSource(spec: CorpusToken): Promise<TokenSource> {
  switch (spec.kind) {
    case "kubernetes-projected-file": {
      const path = join(makeDirectory(), "token");
      await writeFile(path, `${spec.token}\n`, { mode: 0o600 });
      return projectedToken(path);
    }
    case "google-metadata":
      return metadataSource(spec.token);
    case "person-device-sign-in":
      return {
        read: async (): Promise<SubjectToken> => ({
          token: spec.token,
          tokenType: spec.token_type,
        }),
        describe: () => "a person's sign-in",
      };
    default:
      throw new Error(
        `the corpus names the subject kind "${spec.kind}", which this suite does not know`
      );
  }
}

// biome-ignore-start lint/suspicious/noTemplateCurlyInString: the corpus writes each placeholder
// as `${name}`, and these are that file's own text rather than a template this suite writes.
/** The corpus value with this exchange's own substitution applied, and whether it was one of the
 *  placeholders a form field may carry. */
function resolvePlaceholder(exchange: CorpusExchange, value: string): [string, boolean] {
  switch (value) {
    case "${subject_token}":
      return [exchange.subject.token, true];
    case "${actor_token}":
      return [exchange.actor?.token ?? "", true];
    case "${session}":
      return [exchange.session, true];
    default:
      return [value, false];
  }
}

/** The set equality every assertion of this contract rests on: what the corpus names and what the
 *  client sent or read, neither more nor less. */
function checkNames(what: string, want: readonly string[], got: readonly string[]): void {
  expect(
    [...got].sort(),
    `${what} must carry exactly what the corpus names: a field one client sends or reads must be in testdata/wire.json, or the other client is held to nothing about it`
  ).toEqual([...want].sort());
}

/** The proof's header and claim sets whole, the literal claims, and the shape of each
 *  placeholder — including that the embedded key carries no private member, which is the whole of
 *  what the broker may ever receive of a client's key. */
function checkProof(exchange: CorpusExchange, endpoint: string, request: StubRequest): void {
  checkNames(
    "the proof's header",
    Object.keys(exchange.proof.header),
    Object.keys(request.proofHeader)
  );
  checkNames(
    "the proof's claims",
    Object.keys(exchange.proof.claims),
    Object.keys(request.proofClaims)
  );
  for (const [name, want] of Object.entries(exchange.proof.header)) {
    if (name === "jwk") {
      const jwk = request.proofHeader.jwk as Record<string, unknown>;
      expect(
        Object.hasOwn(jwk, "d"),
        "the proof's embedded key carries a d member: the broker must receive the public half and nothing else"
      ).toBe(false);
      expect(jwk.kty).toBe("EC");
      expect(jwk.crv).toBe("P-256");
      expect(typeof jwk.x === "string" && jwk.x.length > 0).toBe(true);
      expect(typeof jwk.y === "string" && jwk.y.length > 0).toBe(true);
      continue;
    }
    expect(request.proofHeader[name]).toBe(want as never);
  }
  for (const [name, want] of Object.entries(exchange.proof.claims)) {
    const got = request.proofClaims[name];
    switch (want) {
      case "${jti}":
        expect(typeof got === "string" && got.length > 0).toBe(true);
        break;
      case "${iat}":
        expect(typeof got).toBe("number");
        expect(Math.abs(Date.now() / 1000 - (got as number))).toBeLessThan(60);
        break;
      case "${token_endpoint}":
        expect(got).toBe(endpoint);
        break;
      default:
        expect(got).toBe(want as never);
    }
  }
}
// biome-ignore-end lint/suspicious/noTemplateCurlyInString: the corpus's placeholders end here.

/** One trade held to the corpus entry: the endpoint, the whole form, and the whole proof. */
function checkRequest(exchange: CorpusExchange, brokerUrl: string, request: StubRequest): void {
  expect(`${request.method} ${request.path}`).toBe(
    `${corpus.endpoint.method} ${corpus.endpoint.path}`
  );
  expect(request.contentType.split(";")[0]).toBe(corpus.endpoint.content_type);
  const names = [...new Set(request.form.keys())];
  for (const name of names) {
    expect(
      request.form.getAll(name).length,
      `the trade carries ${name} more than once, and the corpus names one value for each field`
    ).toBe(1);
  }
  checkNames("the trade's form", Object.keys(exchange.form), names);
  for (const [name, corpusValue] of Object.entries(exchange.form)) {
    const [want] = resolvePlaceholder(exchange, corpusValue);
    expect(request.form.get(name), `the trade's ${name}`).toBe(want);
  }
  checkProof(exchange, brokerUrl + TOKEN_PATH, request);
}

/** A token in the corpus's own names and values: `expires_in` is the seconds left, rounded, since
 *  a client reads a lifetime and holds an instant, and a credential expiry that the answer did not
 *  carry is named by nothing. */
function tokenAsCorpus(token: BrokerToken): Record<string, unknown> {
  const read: Record<string, unknown> = {};
  for (const [field, value] of Object.entries(token)) {
    const name = TOKEN_FIELD_NAMES[field as keyof BrokerToken];
    if (name === undefined) {
      throw new Error(
        `BrokerToken has the field ${field}, which testdata/wire.json names nowhere: add it to the corpus, so the Go client is held to it too`
      );
    }
    if (name === "expires_in") {
      read[name] = Math.round(((value as Date).getTime() - Date.now()) / 1000);
    } else if (name === "credential_expires_at") {
      if (value === null) continue;
      read[name] = (value as Date).toISOString().replace(/\.\d{3}Z$/, "Z");
    } else {
      read[name] = value;
    }
  }
  return read;
}

test("the wire corpus holds this client", async () => {
  for (const exchange of corpus.exchanges) {
    const broker = stubBroker(exchange.answer.status, exchange.answer.body);
    const client = new BrokerTokenClient({
      brokerUrl: broker.url,
      key: await newDpopKey(),
      subject: await corpusSource(exchange.subject),
      ...(exchange.actor === undefined ? {} : { actor: await corpusSource(exchange.actor) }),
      session: exchange.session,
      // The entry's own form says whether this trade declines the launcher credential, so the
      // one place the corpus states it is the form it asserts.
      launcherCredential:
        exchange.form[FORM_LAUNCHER_CREDENTIAL] !== FORM_LAUNCHER_CREDENTIAL_DECLINED,
    });
    let token: BrokerToken | undefined;
    let refusal: unknown;
    try {
      token = await client.token();
    } catch (error) {
      refusal = error;
    }
    expect(broker.requests.length, `${exchange.name}: the client's trades`).toBe(1);
    checkRequest(exchange, broker.url, broker.requests[0] as StubRequest);
    if (exchange.expect.outcome === "accept") {
      expect(refusal, `${exchange.name}: ${refusal}`).toBeUndefined();
      const got = tokenAsCorpus(token as BrokerToken);
      const want = exchange.expect.token as Record<string, unknown>;
      checkNames("the token the client took", Object.keys(want), Object.keys(got));
      for (const [name, value] of Object.entries(want)) {
        expect(String(got[name]), `${exchange.name}: the client read ${name}`).toBe(String(value));
      }
      continue;
    }
    expect(exchange.expect.outcome, `${exchange.name}: this suite knows accept and refuse`).toBe(
      "refuse"
    );
    expect(token, `${exchange.name}: a refused trade hands its caller no identity at all`).toBe(
      undefined
    );
    expect((refusal as Error).message).toContain(exchange.expect.error_contains as string);
    expect(
      refusal instanceof BrokerRefusedError && refusal.permanent,
      `${exchange.name}: only a refusal no later trade can cure stops a renewal loop, and the broker's 429 is not one`
    ).toBe(exchange.expect.permanent as boolean);
  }
});

test("the corpus names every field a client reads", () => {
  const named = new Set<string>();
  for (const exchange of corpus.exchanges) {
    for (const name of Object.keys(exchange.expect.token ?? {})) named.add(name);
  }
  for (const name of Object.values(TOKEN_FIELD_NAMES)) {
    expect(
      named.has(name),
      `no corpus entry names "${name}" in its expect.token, so the Go client is held to nothing about it`
    ).toBe(true);
  }
});
