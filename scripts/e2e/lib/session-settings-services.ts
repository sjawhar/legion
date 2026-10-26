// Dispatch, the Envoy listener and the NATS bus, as the dispatch-session-settings driver reaches
// them: a person's and an agent's sends, the stored thread, the listener's sessions and messages,
// and bare bus clients.
import { readFileSync } from "node:fs";
import { connect } from "nats";
import { agentStreamControlSubject } from "../../../packages/contracts/src/agent-stream";
import { expect, PERSON, type Rig, sleep, until } from "./session-settings-common";

export interface StoredMessage {
  readonly id: string;
  readonly body: string;
  readonly author: { readonly kind: string; readonly id: string };
  readonly created_at: string;
  readonly in_reply_to: string | null;
  readonly deliveries: readonly {
    readonly attempt: number;
    readonly state: string;
    readonly error: string | null;
    readonly accepted_as: string | null;
  }[];
}
export interface StoredRead {
  readonly message: StoredMessage;
  readonly replies: readonly StoredMessage[];
}

export class Services {
  readonly #rig: Rig;
  constructor(rig: Rig) {
    this.#rig = rig;
  }

  async #json(url: string, init: RequestInit): Promise<{ status: number; body: unknown }> {
    const response = await fetch(url, { ...init, signal: AbortSignal.timeout(30_000) });
    const raw = await response.text();
    let body: unknown = raw;
    try {
      body = JSON.parse(raw);
    } catch {
      // A body that is not JSON is kept as its text.
    }
    return { body, status: response.status };
  }

  /** What `login` reads of the session's stored thread, as the page reads it. */
  async thread(session: string, login = PERSON): Promise<StoredRead[]> {
    const { body, status } = await this.#json(
      `${this.#rig.dispatchUrl}/api/v1/agents/${encodeURIComponent(session)}/messages`,
      { headers: { "X-Dispatch-User": login } }
    );
    expect(
      status === 200,
      `GET the stored thread of ${session} answered ${status}: ${JSON.stringify(body)}`
    );
    return body as StoredRead[];
  }

  /** A person's Send to the session, as the composer and the header send it. */
  async send(session: string, body: string, login: string): Promise<StoredMessage> {
    const answer = await this.#json(
      `${this.#rig.dispatchUrl}/api/v1/agents/${encodeURIComponent(session)}/messages`,
      {
        body: JSON.stringify({ body, delivery: "steer" }),
        headers: { "Content-Type": "application/json", "X-Dispatch-User": login },
        method: "POST",
      }
    );
    expect(
      answer.status === 201,
      `${login}'s Send of "${body}" answered ${answer.status}: ${JSON.stringify(answer.body)}`
    );
    return answer.body as StoredMessage;
  }

  /** `path` posted with the session's own Dispatch bearer, as an agent would post it. */
  async #asAgent(path: string, body: object): Promise<{ status: number; body: unknown }> {
    const token = readFileSync(this.#rig.dispatchTokenFile, "utf8").trim();
    return this.#json(`${this.#rig.dispatchUrl}${path}`, {
      body: JSON.stringify(body),
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      method: "POST",
    });
  }

  /** The same Send with the session's own Dispatch bearer, as an agent would make it. */
  sendAsAgent(session: string, body: string): Promise<{ status: number; body: unknown }> {
    return this.#asAgent(`/api/v1/agents/${encodeURIComponent(session)}/messages`, {
      body,
      delivery: "steer",
    });
  }

  /** The session's accept of an attempt as its user's turn, asked as the session asks it. */
  accept(id: string, attempt: number, session: string): Promise<{ status: number; body: unknown }> {
    return this.#asAgent(`/api/v1/messages/${id}/deliveries/${attempt}/accept`, {
      actor: { id: session, kind: "session" },
    });
  }

  async #listener(path: string, body?: object): Promise<unknown> {
    const token = readFileSync(this.#rig.envoyTokenFile, "utf8").trim();
    const answer = await this.#json(`${this.#rig.envoyUrl}${path}`, {
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      ...(body === undefined ? {} : { body: JSON.stringify(body), method: "POST" }),
    });
    expect(
      answer.status === 200,
      `the listener answered ${path} with ${answer.status}: ${JSON.stringify(answer.body)}`
    );
    return answer.body;
  }

  async registered(session: string, since = 0): Promise<boolean> {
    const sessions = (await this.#listener("/v1/sessions")) as {
      session_id: string;
      last_seen: number;
    }[];
    return sessions.some((row) => row.session_id === session && row.last_seen >= since);
  }

  /** Registers an Envoy client of the run's own, as any session on the bus registers. */
  async registerClient(session: string): Promise<void> {
    await this.#listener("/v1/interests/subscribe", {
      dir: this.#rig.work,
      self_subscribed: true,
      session_id: session,
      title: "INFRA-195 rig sender",
      topics: [`notifications.agent.${session}`],
    });
  }

  /** An Envoy message from `from` to the session, as one session sends another. */
  async envoyMessage(from: string, to: string, message: string, key: string): Promise<void> {
    await this.#listener("/v1/messages/send", {
      idempotency_key: key,
      message,
      source: "agent",
      source_session: from,
      target_session: to,
    });
  }

  /** A Dispatch-shaped frame naming `messageId` at attempt 1 as alice's steer on no issue,
   *  carrying `body`: what any holder of the listener token can write
   *  (dispatch-user-turns.sh's forged_frame), sent to the session the listener lists. */
  async forgeDispatchFrame(
    session: string,
    messageId: string,
    body: string,
    key: string
  ): Promise<void> {
    const payload = JSON.stringify({
      delivery: { attempt: 1, mode: "steer" },
      event: {
        actor: { id: PERSON, kind: "user" },
        issue_key: null,
        payload: {
          author: { id: PERSON, kind: "user" },
          body,
          created_at: new Date().toISOString(),
          deliveries: [],
          id: messageId,
          in_reply_to: null,
          issue_key: null,
          target: `session:${session}`,
        },
        type: "message.created",
      },
    });
    await this.#listener("/v1/messages/send", {
      idempotency_key: key,
      message: body,
      payload,
      source: "dispatch",
      target_session: session,
    });
  }

  /** Publishes `frame` on the session's frames subject as a bare bus client. */
  async publishFrame(session: string, frame: object): Promise<void> {
    const nc = await connect({
      name: "legion-e2e-session-settings",
      servers: this.#rig.natsUrl,
      timeout: 10_000,
    });
    try {
      nc.publish(`agentstream.${session}.frames`, new TextEncoder().encode(JSON.stringify(frame)));
      await nc.flush();
    } finally {
      await nc.close();
    }
  }

  /** Subscribes to the session's control subject as a bare bus client and answers the first
   *  message that arrives there within `limitMs` as text (the relay's `watch` while a page is
   *  open), or undefined when none does. */
  async nextControl(session: string, limitMs: number): Promise<string | undefined> {
    const nc = await connect({
      name: "legion-e2e-session-settings",
      servers: this.#rig.natsUrl,
      timeout: 10_000,
    });
    try {
      const subscription = nc.subscribe(agentStreamControlSubject(session), { max: 1 });
      await nc.flush();
      const first = async () => {
        for await (const message of subscription) return new TextDecoder().decode(message.data);
        return undefined;
      };
      return await Promise.race([first(), sleep(limitMs).then(() => undefined)]);
    } finally {
      await nc.close();
    }
  }
}

/** The message `body` that `login` sent the session after the run had seen `before`. */
export async function newMessage(
  services: Services,
  session: string,
  body: string,
  before: ReadonlySet<string>
): Promise<StoredRead> {
  return until(`Dispatch to store "${body}" to ${session}`, 30_000, async () =>
    (await services.thread(session)).find(
      (read) => read.message.body === body && !before.has(read.message.id)
    )
  );
}
export async function storedIds(services: Services, session: string): Promise<Set<string>> {
  return new Set((await services.thread(session)).map((read) => read.message.id));
}
/** The session's reply to stored message `id`, once there is one. */
export async function sessionReply(
  services: Services,
  session: string,
  id: string,
  limitMs: number
): Promise<string> {
  return until(`the session's reply to message ${id}`, limitMs, async () => {
    const read = (await services.thread(session)).find((each) => each.message.id === id);
    return read?.replies.find((reply) => reply.author.kind === "session")?.body;
  });
}
