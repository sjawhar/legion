import { inboundTimestamp } from "@legion/envoy-client/delivery";
import { encode } from "@toon-format/toon";
import { REPLAY_DELIVERED_ENTRY, writtenCardIdentity } from "./missed-messages";

/**
 * How long a card may take from its send to this session before its card states the wait in
 * words. A card read that late carries an `at:` stamp that far behind the present, and a reader
 * that takes the stamp for the present time draws wrong conclusions from it, about the clocks of
 * the machines that sent it among others.
 */
export const MATERIAL_WAIT_MS = 15 * 60_000;

/** The most cards whose delivery time `CardAges` keeps; past it the oldest is forgotten first. */
export const CARD_AGES_LIMIT = 20_000;

/** What `CardAges` asks of the extension around it. */
export interface CardAgesDeps {
  readonly now: () => number;
}

/** A card in a request's messages: its event id, and the text that holds its `envoy:` block. */
interface CardText {
  readonly eventID: string;
  readonly text: string;
  /** The message with that text replaced: a copy, since the host keeps the original. */
  readonly withText: (text: string) => unknown;
}

/**
 * The time each Envoy card reached this session, and each card's text in a model request with it.
 *
 * A card's `envoy:` block carries `at:`, the time its sender sent it. The host writes the card
 * when it takes it into the session, which can be much later: on a session whose inbox ran behind,
 * hours later. So every request the session makes shows each card with a second time,
 * `delivered:`, the time the host wrote it, and, when the two are `MATERIAL_WAIT_MS` or more apart,
 * `waited:`, the gap in words. The card the host persists is unchanged: these lines exist only in
 * the requests (`annotate`, from the extension's `context` handler).
 *
 * The delivery time of a card is the time the host emitted its `message_end` in this process
 * (`written`). A card whose `message_end` this process did not see before its first request has
 * the time its replay ledger record was written (`REPLAY_DELIVERED_ENTRY`, its `at`), when the
 * session's entries hold one, else the time of that first request, when the host handed the card
 * to this process (`timestamp`) after this instance started. A card known by none of them keeps
 * its text as it is.
 *
 * A request's bytes up to the newest message repeat on the next request, so the provider's prompt
 * cache can read them again: each card's delivery time is fixed the first time it is used and
 * reused after, so a card's text is the same in every request this process makes, up to the
 * `CARD_AGES_LIMIT` newest cards. A restart takes the time from the ledger record, written within
 * the same moment as the `message_end`; to the second they agree, except for a write that lands in
 * the next second, which re-reads the cached prefix once from that card on.
 */
export class CardAges {
  readonly #deps: CardAgesDeps;
  readonly #startedAt: number;
  readonly #delivered = new Map<string, number>();

  constructor(deps: CardAgesDeps) {
    this.#deps = deps;
    this.#startedAt = deps.now();
  }

  /** The host wrote `message`: a card's first write fixes its delivery time. */
  written(message: unknown): void {
    const eventID = writtenCardIdentity(message)?.event_id;
    if (eventID === undefined || this.#delivered.has(eventID)) return;
    this.#remember(eventID, this.#deps.now());
  }

  /**
   * `messages`, one model request's, with each card's delivery time and, when material, its wait;
   * undefined when no card changed. `branch` is the session's entries, read only when a card's
   * delivery time is not known yet.
   */
  annotate(messages: readonly unknown[], branch: () => readonly unknown[]): unknown[] | undefined {
    let recorded: Map<string, number> | undefined;
    let changed = false;
    const annotated = messages.map((message) => {
      const card = cardText(message);
      if (card === undefined) return message;
      let delivered = this.#delivered.get(card.eventID);
      if (delivered === undefined) {
        recorded ??= ledgerTimes(branch());
        delivered = recorded.get(card.eventID) ?? this.#handedHere(message);
        if (delivered === undefined) return message;
        this.#remember(card.eventID, delivered);
      }
      const text = withDelivery(card.text, delivered);
      if (text === undefined) return message;
      changed = true;
      return card.withText(text);
    });
    return changed ? annotated : undefined;
  }

  /** Now, for a card the host was handed after this instance started; undefined otherwise. */
  #handedHere(message: unknown): number | undefined {
    const handed =
      typeof message === "object" && message !== null && "timestamp" in message
        ? message.timestamp
        : undefined;
    return typeof handed === "number" && handed >= this.#startedAt ? this.#deps.now() : undefined;
  }

  #remember(eventID: string, at: number): void {
    this.#delivered.set(eventID, at);
    if (this.#delivered.size > CARD_AGES_LIMIT) {
      const oldest = this.#delivered.keys().next();
      if (oldest.done !== true) this.#delivered.delete(oldest.value);
    }
  }
}

/** The time each event's ledger record was written (`ReplayLedgerEntry.at`), from the entries. */
function ledgerTimes(entries: readonly unknown[]): Map<string, number> {
  const times = new Map<string, number>();
  for (const entry of entries) {
    if (typeof entry !== "object" || entry === null) continue;
    if (!("type" in entry) || entry.type !== "custom") continue;
    if (!("customType" in entry) || entry.customType !== REPLAY_DELIVERED_ENTRY) continue;
    const data: unknown = "data" in entry ? entry.data : undefined;
    if (typeof data !== "object" || data === null) continue;
    const eventID = "event_id" in data ? data.event_id : undefined;
    const at = "at" in data ? data.at : undefined;
    if (typeof eventID !== "string" || typeof at !== "number" || times.has(eventID)) continue;
    times.set(eventID, at);
  }
  return times;
}

/** The card `message` is, when it is an `envoy-message` card naming an event with an `envoy:` block. */
function cardText(message: unknown): CardText | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  if (!("role" in message) || message.role !== "custom") return undefined;
  const eventID = writtenCardIdentity(message)?.event_id;
  if (eventID === undefined) return undefined;
  const content: unknown = "content" in message ? message.content : undefined;
  if (typeof content === "string") {
    if (!content.startsWith("envoy:\n")) return undefined;
    return { eventID, text: content, withText: (text) => ({ ...message, content: text }) };
  }
  if (!Array.isArray(content)) return undefined;
  for (const [block, part] of content.entries()) {
    if (typeof part !== "object" || part === null) continue;
    const text = "text" in part ? part.text : undefined;
    if (typeof text !== "string" || !text.startsWith("envoy:\n")) continue;
    return {
      eventID,
      text,
      withText: (replaced) => ({
        ...message,
        content: content.map((other, index) =>
          index === block ? { ...part, text: replaced } : other
        ),
      }),
    };
  }
  return undefined;
}

/** The `at:` line of an `envoy:` block: two spaces in, as `renderInbound` encodes it. */
const AT_LINE = /^ {2}at: (.+)$/;

/** The `delivered:` line this module adds, which another copy of the extension may have added. */
const DELIVERED_LINE = /^ {2}delivered: /;

/**
 * A card's text with `delivered:`, and `waited:` when material, after its `at:` line; undefined
 * for a text with no `at:` line, or one that already says when it was delivered.
 */
export function withDelivery(text: string, delivered: number): string | undefined {
  const lines = text.split("\n");
  const atIndex = lines.findIndex((line) => AT_LINE.test(line));
  if (atIndex < 0 || lines.some((line) => DELIVERED_LINE.test(line))) return undefined;
  const sent = Date.parse(toonString(AT_LINE.exec(lines[atIndex] ?? "")?.[1] ?? ""));
  const waitedMs = Number.isNaN(sent) ? 0 : delivered - sent;
  const fields = {
    delivered: inboundTimestamp(delivered),
    ...(waitedMs >= MATERIAL_WAIT_MS && {
      waited: `${duration(waitedMs)} between at and delivered; at is when it was sent, not the present time`,
    }),
  };
  const added = encode(fields)
    .split("\n")
    .map((line) => `  ${line}`);
  return [...lines.slice(0, atIndex + 1), ...added, ...lines.slice(atIndex + 1)].join("\n");
}

/** A TOON value as a string: a quoted one is JSON. */
function toonString(raw: string): string {
  if (!raw.startsWith('"')) return raw;
  try {
    const value: unknown = JSON.parse(raw);
    return typeof value === "string" ? value : raw;
  } catch {
    return raw;
  }
}

/** A wait in whole minutes: `52 min`, or `4 h 45 min`. */
export function duration(ms: number): string {
  const minutes = Math.floor(ms / 60_000);
  const hours = Math.floor(minutes / 60);
  return hours === 0 ? `${minutes} min` : `${hours} h ${minutes % 60} min`;
}
