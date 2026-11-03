// packages/broker-client/src/token-client.ts
//
// This repository's TypeScript client of the secrets broker's RFC 8693 token exchange: a machine
// trades the platform identity it already holds for one of the broker's access tokens, which
// Dispatch and the Envoy listener read back at the broker's introspection route. The Go client of
// `packages/envoy/internal/brokerclient` is its other half, and the two are held to one wire by
// the corpus in that package's `testdata/wire.json`, which both suites replay.
//
// What the exchange is: the caller presents the subject token its platform already gives it (a
// `TokenSource`), proves a key of its own with a DPoP proof (RFC 9449), and is answered an access
// token bound to that key. The answer carries no refresh token at all — the grant issues none —
// so a renewal is a fresh subject token traded again, which is why a source is read at every
// trade rather than once.
//
// What it refuses to do: hand out a token it could not renew. `token()` answers the token in hand
// while that token is live and the broker is merely unreachable, busy (its 429) or refusing this
// moment's proof (`invalid_dpop_proof`, `use_dpop_nonce`), and throws once the broker has refused
// the identity in a way no retry can cure or the token in hand has run out, so a caller that
// writes only with what `token()` gave it never writes with no identity.

import {
  allowInsecureRequests,
  Configuration,
  type CustomFetchOptions,
  customFetch,
  genericGrantRequest,
  None,
  ResponseBodyError,
  type TokenEndpointResponse,
} from "openid-client";
import type { DpopKey } from "./dpop";
import type { SubjectToken, TokenSource } from "./source";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  EXCHANGE_CLIENT_ID,
  FORM_LAUNCHER_CREDENTIAL,
  FORM_LAUNCHER_CREDENTIAL_DECLINED,
  FORM_SESSION,
  GRANT_TYPE_TOKEN_EXCHANGE,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  TOKEN_PATH,
  TOKEN_TYPE_DPOP,
} from "./wire";

/**
 * How long before an access token's expiry the client trades again, so a renewal lands with the
 * token in hand still good and nothing of the caller's restarts. The broker mints a machine's
 * token for an hour or less.
 */
export const DEFAULT_RENEW_BEFORE_MS = 5 * 60_000;

/** One of the broker's access tokens as this client holds it. The five members here are the whole
 *  of what a client reads out of an answer, as `testdata/wire.json`'s `expect.token` names them. */
export interface BrokerToken {
  /** The token itself, presented as `Authorization: DPoP <token>` beside a proof of the key it is
   *  bound to. */
  readonly accessToken: string;
  /** The answer's `token_type`, `DPoP` for every token this grant mints. */
  readonly tokenType: string;
  /** When the token lapses, from the answer's `expires_in`. */
  readonly expiresAt: Date;
  /** The launcher credential the trade minted for this machine or sign-in, under which sessions
   *  of this machine enrol; empty where the answer carries none. */
  readonly credentialId: string;
  /** When that credential lapses, null where the answer carries none. */
  readonly credentialExpiresAt: Date | null;
}

/** The broker's rate limit: the first of the three refusals a later trade may be answered
 *  differently. */
const TOO_MANY_REQUESTS = 429;

/** What the broker answers a proof whose `iat` is outside its 60 s skew window. */
const INVALID_DPOP_PROOF = "invalid_dpop_proof";

/** What the broker answers a proof it wants a nonce in (RFC 9449 Section 8), which `#send`
 *  already retries once with the nonce the answer carried. */
const USE_DPOP_NONCE = "use_dpop_nonce";

/**
 * A trade the broker refused: a plain error naming what it said. Almost every refusal is one no
 * retry can cure — an identity no entry lists, a sign-in a person revoked, a token of the wrong
 * audience — and a renewal stops on it rather than asking again. The three exceptions are the
 * broker's 429 and its two DPoP refusals (`permanent`).
 */
export class BrokerRefusedError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly description: string
  ) {
    super(
      description === ""
        ? `the secrets broker refused this machine's identity: HTTP ${status} ${code}`
        : `the secrets broker refused this machine's identity: HTTP ${status} ${code}: ${description}`
    );
    this.name = "BrokerRefusedError";
  }

  /**
   * Whether this refusal is one no retry can cure, which every refusal is but three. The Go
   * client reads it the same way, out of the same three cases (`RefusedError.Permanent`,
   * packages/envoy/internal/brokerclient/brokerclient.go), and
   * `packages/envoy/internal/brokerclient/testdata/wire.json` holds the two to that one reading.
   *
   * The broker's 429: its token endpoint spends a bucket per source address, and a bucket
   * refills. A client that dropped the token in hand on a 429 would write with no identity
   * through a burst — its own, or another process's on the same machine — while the token it
   * already held was still good.
   *
   * `invalid_dpop_proof`, which is what the broker answers a proof whose `iat` is outside its
   * 60 s skew window. A process started before NTP has corrected the host's clock would
   * otherwise drop a live token and fail every Dispatch and listener call until something
   * restarted it; the next trade after the correction succeeds.
   *
   * `use_dpop_nonce`, which `#send` already retries once with the nonce the answer carried. One
   * that reaches here is an answer that carried no nonce, or a nonce the broker then refused in
   * turn — a server state the next trade can find changed.
   */
  get permanent(): boolean {
    return (
      this.status !== TOO_MANY_REQUESTS &&
      this.code !== INVALID_DPOP_PROOF &&
      this.code !== USE_DPOP_NONCE
    );
  }
}

/** One client: the broker it trades at, the identity it trades, and the key it proves. */
export interface BrokerTokenClientOptions {
  /** The broker's base URL, with or without a trailing slash. */
  readonly brokerUrl: string;
  /** This process's own DPoP key, whose public half alone reaches the broker. */
  readonly key: DpopKey;
  /** The identity being traded. */
  readonly subject: TokenSource;
  /** The machine a person signed in on, presented as the actor token beside their own Google
   *  identity token. Absent for every trade a machine makes for itself. */
  readonly actor?: TokenSource | undefined;
  /** The session name the minted token writes under, which an entry that fixes one refuses to be
   *  told and an entry that fixes none requires. */
  readonly session?: string | undefined;
  /**
   * Whether this trade wants the launcher credential its identity holds minted or moved onto
   * the key it proved ({@link FORM_LAUNCHER_CREDENTIAL}); true by default, as an absent
   * parameter is on the wire.
   *
   * Every caller in this repository passes `false` (`brokerTokenClient`, ./environment.ts): the
   * TypeScript clients want a Dispatch access token and nothing else, and none of them reads
   * `credentialId`. Each trade that asks writes a `launcher_credentials` row keyed by the
   * machine and the key it proved and a `launcher_credential.token_login` audit row, and this
   * client makes a key per process and per session, so asking would put a row on an operator's
   * machine-logins list for every `dispatch` call, every hook run and every session of a
   * long-lived process — rows nothing removes.
   */
  readonly launcherCredential?: boolean | undefined;
  /** Overrides {@link DEFAULT_RENEW_BEFORE_MS}. Only a test shortens it. */
  readonly renewBeforeMs?: number | undefined;
  /** `Date.now` unless a test replaces it. */
  readonly now?: (() => number) | undefined;
  /** The fetch every call goes through; the global one by default. The DPoP proof is signed
   *  around it, so a caller passing its own keeps whatever that one does. */
  readonly fetch?: typeof fetch | undefined;
}

/**
 * The HTTP status behind `error`, or 0 where it carries none: a status the library reports as an
 * unconforming answer is left in the response it hangs off the error's `cause` chain.
 */
function statusOf(error: unknown): number {
  let cause: unknown = error;
  for (let depth = 0; depth < 4; depth += 1) {
    if (cause instanceof Response) return cause.status;
    if (typeof cause !== "object" || cause === null || !("cause" in cause)) return 0;
    cause = cause.cause;
  }
  return 0;
}

/**
 * `url` without the slashes it ends in, counted from the right. A `/\/+$/` replace reads the same
 * string once per position its unanchored search starts at, which costs a URL of n slashes O(n²)
 * (CodeQL js/polynomial-redos); this reads it once.
 */
function withoutTrailingSlashes(url: string): string {
  let end = url.length;
  while (end > 0 && url[end - 1] === "/") end -= 1;
  return url.slice(0, end);
}

/** Trades this client's identity for the broker's access tokens and keeps one live. */
export class BrokerTokenClient {
  readonly #brokerUrl: string;
  readonly #key: DpopKey;
  readonly #subject: TokenSource;
  readonly #actor: TokenSource | undefined;
  readonly #session: string;
  readonly #launcherCredential: boolean;
  readonly #renewBeforeMs: number;
  readonly #now: () => number;
  readonly #fetch: typeof fetch;
  /** The most recent `DPoP-Nonce` the broker sent, carried in every later proof. */
  #nonce = "";
  #held: BrokerToken | null = null;
  /** The trade under way, so two callers that need a token at once make one trade. */
  #trading: Promise<BrokerToken> | null = null;

  constructor(options: BrokerTokenClientOptions) {
    const brokerUrl = withoutTrailingSlashes(options.brokerUrl.trim());
    if (brokerUrl === "") throw new Error("broker client: no broker URL");
    this.#brokerUrl = brokerUrl;
    this.#key = options.key;
    this.#subject = options.subject;
    this.#actor = options.actor;
    this.#session = options.session ?? "";
    this.#launcherCredential = options.launcherCredential ?? true;
    this.#renewBeforeMs = options.renewBeforeMs ?? DEFAULT_RENEW_BEFORE_MS;
    this.#now = options.now ?? Date.now;
    this.#fetch = options.fetch ?? fetch;
  }

  /** The key this client proves, whose public half the broker records. */
  get key(): DpopKey {
    return this.#key;
  }

  /** Names the identity traded, without naming a token. */
  describe(): string {
    return this.#subject.describe();
  }

  /**
   * A live access token: the one in hand while it is good for longer than the renewal window, and
   * otherwise a fresh trade. A trade the broker did not answer, or answered with one of the three
   * refusals a later trade may be answered differently (`BrokerRefusedError.permanent`), while the
   * token in hand is still live answers that token, since a client that already holds an identity
   * need not stop for a broker that is merely down, busy, or refusing this moment's proof; a
   * refusal no retry can cure, and a failure with nothing live left, throw, so no caller writes
   * with no identity.
   */
  async token(signal?: AbortSignal): Promise<BrokerToken> {
    const held = this.#held;
    if (held !== null && this.#now() < held.expiresAt.getTime() - this.#renewBeforeMs) return held;
    if (this.#trading !== null) return await this.#trading;
    const trading = this.#renew(signal);
    this.#trading = trading;
    try {
      return await trading;
    } finally {
      this.#trading = null;
    }
  }

  /** One trade, with what the client does about a failure of it. */
  async #renew(signal: AbortSignal | undefined): Promise<BrokerToken> {
    try {
      const token = await this.#trade(signal);
      this.#held = token;
      return token;
    } catch (error) {
      const held = this.#held;
      if (
        !(error instanceof BrokerRefusedError && error.permanent) &&
        held !== null &&
        this.#now() < held.expiresAt.getTime()
      ) {
        return held;
      }
      this.#held = null;
      throw error;
    }
  }

  /**
   * One RFC 8693 exchange: the subject token this moment's source answers, the actor token where a
   * person is signing in, the session name, whether this trade declines the launcher credential
   * its identity holds, and a DPoP proof of this client's key.
   *
   * `openid-client` makes the request and reads the answer — the grant type, the client id, the
   * form encoding, the endpoint and the shape of what came back — and the proof is signed by the
   * fetch it is given, so one signer serves the token endpoint and every later request this
   * process makes with the token it got.
   */
  async #trade(signal: AbortSignal | undefined): Promise<BrokerToken> {
    const subject = await this.#subject.read(signal);
    const form: Record<string, string> = {
      subject_token: subject.token,
      subject_token_type: subject.tokenType,
    };
    if (this.#session !== "") form[FORM_SESSION] = this.#session;
    if (!this.#launcherCredential) {
      // Sent only to decline: an absent parameter asks, so a broker that has never heard of this
      // parameter answers a client that does ask exactly as it always has.
      form[FORM_LAUNCHER_CREDENTIAL] = FORM_LAUNCHER_CREDENTIAL_DECLINED;
    }
    if (this.#actor !== undefined) {
      let actor: SubjectToken;
      try {
        actor = await this.#actor.read(signal);
      } catch (error) {
        const said = error instanceof Error ? error.message : String(error);
        throw new Error(`read the machine this person signed in on: ${said}`, { cause: error });
      }
      form["actor_token"] = actor.token;
      // The same token under the type that says what it proves here: not who is asking, but which
      // box the person is asking from.
      form["actor_token_type"] =
        actor.tokenType === SUBJECT_TOKEN_TYPE_GOOGLE ? ACTOR_TOKEN_TYPE_MACHINE : actor.tokenType;
    }
    const endpoint = this.#brokerUrl + TOKEN_PATH;
    const raw: { body: unknown } = { body: undefined };
    const config = new Configuration(
      { issuer: this.#brokerUrl, token_endpoint: endpoint },
      EXCHANGE_CLIENT_ID,
      {},
      None()
    );
    // The broker is wherever the deployment put it, which in a cluster and on a dev stack is an
    // http origin; the client posts where it is told, as the Go client does.
    allowInsecureRequests(config);
    config[customFetch] = (url, options) => this.#send(url, options, signal, raw);
    let answered: TokenEndpointResponse;
    try {
      answered = await genericGrantRequest(config, GRANT_TYPE_TOKEN_EXCHANGE, form);
    } catch (error) {
      throw this.#refusal(endpoint, error);
    }
    return this.#read(endpoint, answered, raw.body);
  }

  /**
   * What a failed trade is. An OAuth error the broker answered with a 4xx is a refusal of the
   * identity, which no retry can cure; everything else — a broker that did not answer, a 5xx, an
   * answer that is not this grant's — is an error a renewal may ask about again. A status the
   * library reports without naming it is named here, since "the broker answered HTTP 503" is what
   * tells a reader to look at the broker rather than at this process.
   */
  #refusal(endpoint: string, error: unknown): Error {
    if (error instanceof ResponseBodyError && error.status >= 400 && error.status < 500) {
      return new BrokerRefusedError(error.status, error.error, error.error_description ?? "");
    }
    const status = statusOf(error);
    const message = error instanceof Error ? error.message : String(error);
    const said = status === 0 ? message : `HTTP ${status}: ${message}`;
    return new Error(`trade ${this.#subject.describe()} at ${endpoint}: ${said}`, {
      cause: error,
    });
  }

  /**
   * One request through the caller's fetch with a DPoP proof of this client's key, retried once
   * when the broker answers `use_dpop_nonce` with a nonce to carry (RFC 9449 Section 8). The
   * answer's own body is kept in `raw`, since `openid-client` lowercases `token_type` (RFC 6749
   * makes it case-insensitive) while the corpus names the spelling the broker wrote.
   */
  async #send(
    url: string,
    options: CustomFetchOptions,
    signal: AbortSignal | undefined,
    raw: { body: unknown }
  ): Promise<Response> {
    const send = async (): Promise<Response> => {
      const headers = new Headers(options.headers);
      headers.set(
        "DPoP",
        await this.#key.proof({ method: options.method, url, nonce: this.#nonce })
      );
      const signals = [options.signal, signal].filter((one) => one !== undefined);
      // openid-client's own body union is wider than one `fetch` typing or another admits; the
      // bytes are the form it encoded, and every runtime this package runs on sends them.
      const body = options.body as RequestInit["body"];
      return await this.#fetch(url, {
        method: options.method,
        headers,
        body,
        redirect: options.redirect,
        ...(signals.length === 0 ? {} : { signal: AbortSignal.any(signals) }),
      });
    };
    let answer = await send();
    const given = answer.headers.get("DPoP-Nonce") ?? "";
    if (given !== "") this.#nonce = given;
    if (answer.status === 400 && given !== "") {
      const body: unknown = await answer
        .clone()
        .json()
        .catch(() => null);
      const asked =
        typeof body === "object" &&
        body !== null &&
        "error" in body &&
        body.error === "use_dpop_nonce";
      if (asked) answer = await send();
    }
    raw.body = await answer
      .clone()
      .json()
      .catch(() => undefined);
    return answer;
  }

  /**
   * The answer as a token, refusing one that is not this grant's: a token type that is not DPoP,
   * no usable expiry, or a refresh token — the exchange issues none, and a client that kept one
   * would hold a credential this contract says is never minted.
   *
   * Every member but one is read from what `openid-client` validated. `token_type` is read from
   * `raw`, the answer's own body: the library lowercases it, RFC 6749 Section 7.1 making it
   * case-insensitive, while the corpus names the spelling the broker wrote and holds both clients
   * to it.
   */
  #read(endpoint: string, answered: TokenEndpointResponse, raw: unknown): BrokerToken {
    const written =
      typeof raw === "object" && raw !== null && "token_type" in raw ? raw.token_type : undefined;
    const tokenType = typeof written === "string" ? written : "";
    if (tokenType.toLowerCase() !== TOKEN_TYPE_DPOP.toLowerCase()) {
      throw new Error(
        `the broker at ${endpoint} answered token_type ${JSON.stringify(tokenType)}, and this grant mints DPoP-bound tokens alone`
      );
    }
    if (answered.refresh_token !== undefined && answered.refresh_token !== "") {
      throw new Error(
        `the broker at ${endpoint} answered a refresh token, which this grant never issues; a renewal trades a fresh identity token instead`
      );
    }
    const seconds = answered.expires_in ?? 0;
    if (!(seconds > 0)) {
      throw new Error(
        `the broker at ${endpoint} answered no usable expires_in (${JSON.stringify(answered.expires_in)})`
      );
    }
    const expiresAtText =
      typeof answered["credential_expires_at"] === "string"
        ? answered["credential_expires_at"]
        : "";
    let credentialExpiresAt: Date | null = null;
    if (expiresAtText !== "") {
      credentialExpiresAt = new Date(expiresAtText);
      if (Number.isNaN(credentialExpiresAt.getTime())) {
        throw new Error(
          `the broker at ${endpoint} answered a credential_expires_at that is no RFC 3339 time (${JSON.stringify(expiresAtText)})`
        );
      }
    }
    return {
      accessToken: answered.access_token,
      tokenType,
      expiresAt: new Date(this.#now() + seconds * 1000),
      credentialId: typeof answered["credential_id"] === "string" ? answered["credential_id"] : "",
      credentialExpiresAt,
    };
  }
}
