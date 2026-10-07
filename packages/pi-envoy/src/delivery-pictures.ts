import type { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { readPictures, shownPictureAddresses } from "@legion/envoy-client/dispatch-picture-tools";
import { imageBlocks, pictureAddresses } from "@legion/envoy-client/dispatch-pictures";
import type { ContentBlock } from "@legion/pi-shared/pi-types";

/** What an Inbox delivery hands the model, and the pictures it shows. */
export interface PictureDelivery {
  /** `text` alone, or `text` followed by one image block per picture shown. */
  readonly content: string | ContentBlock[];
  /** The addresses of the pictures `content` shows, in its order. They join the session's
   *  `shownPictures` once the host took the delivery: a send that fails leaves them for the
   *  delivery Dispatch retries. */
  readonly shown: readonly string[];
}

/**
 * The content an Inbox delivery hands the model: `text` alone, or, when it embeds pictures Dispatch
 * serves (`addresses`, in writing order), `text` followed by one image block per picture shown,
 * `PICTURES_SHOWN_MAX` and `PICTURES_SHOWN_MAX_BYTES` at most, as `dispatch_read` shows them.
 * `shown` is the pictures this session was already shown (`shownPictures`): one of them is named
 * as shown earlier rather than sent again. `shown` is only read; the caller adds the delivery's
 * `shown` once the host took it. `named` appends the `Pictures:` lines that say which image is
 * which and name the rest, a card whose every picture was shown earlier included; a person's own
 * turn keeps their text exactly as they sent it. A picture Dispatch cannot serve, or no Dispatch
 * configuration (`client` answers undefined or throws), leaves the delivery its text.
 */
export async function withDeliveredPictures(
  text: string,
  addresses: readonly string[],
  client: () => DispatchClient | undefined,
  named: boolean,
  shown: ReadonlySet<string>
): Promise<PictureDelivery> {
  const textAlone = { content: text, shown: [] };
  if (addresses.length === 0) return textAlone;
  let dispatch: DispatchClient | undefined;
  try {
    dispatch = client();
  } catch {
    // A Dispatch configuration that cannot be read costs the delivery its pictures, never its text.
    return textAlone;
  }
  if (dispatch === undefined) return textAlone;
  const unique = [...new Set(addresses)];
  const shownEarlier = unique.some((address) => shown.has(address));
  const pictures = await readPictures(dispatch, unique, shown);
  const content = named ? `${text}\n\n${pictures.lines.join("\n")}` : text;
  if (pictures.images.length === 0) return shownEarlier ? { content, shown: [] } : textAlone;
  return {
    content: [{ type: "text", text: content }, ...imageBlocks(pictures.images)],
    shown: pictures.shown,
  };
}

/** A transcript entry's message, as far as its pictures go. */
interface TranscriptMessage {
  readonly role: unknown;
  readonly content: unknown;
}

/** A transcript entry's message: a message entry's own, or a custom message entry's content. */
function entryMessage(entry: unknown): TranscriptMessage | undefined {
  if (typeof entry !== "object" || entry === null || !("type" in entry)) return undefined;
  if (entry.type === "custom_message") {
    return { role: "custom", content: "content" in entry ? entry.content : undefined };
  }
  if (entry.type !== "message" || !("message" in entry)) return undefined;
  const { message } = entry;
  if (typeof message !== "object" || message === null) return undefined;
  return {
    role: "role" in message ? message.role : undefined,
    content: "content" in message ? message.content : undefined,
  };
}

/**
 * The addresses of the pictures a session's transcript already shows, each once: every picture a
 * tool result or a delivered card names as shown (`shownPictureAddresses`: `dispatch_read`,
 * `dispatch_doc_read`, an Inbox card), and the pictures of a person's own turn whose image blocks
 * number the pictures its text embeds, so every one of them was shown (a turn keeps its text as
 * sent, naming none). `entries` are the transcript as Oh My Pi keeps it: a message entry holds its
 * message, a custom message entry its content.
 */
export function transcriptPictures(entries: readonly unknown[]): string[] {
  const shown = new Set<string>();
  for (const entry of entries) {
    const message = entryMessage(entry);
    if (message === undefined) continue;
    const { content } = message;
    const texts: string[] = [];
    let images = 0;
    if (typeof content === "string") texts.push(content);
    else if (Array.isArray(content)) {
      for (const block of content) {
        if (typeof block !== "object" || block === null || !("type" in block)) continue;
        if (block.type === "image") images++;
        if (block.type === "text" && "text" in block && typeof block.text === "string") {
          texts.push(block.text);
        }
      }
    }
    for (const address of shownPictureAddresses(texts)) shown.add(address);
    if (message.role !== "user" || images === 0) continue;
    const embedded = new Set(texts.flatMap(pictureAddresses));
    if (embedded.size === images) for (const address of embedded) shown.add(address);
  }
  return [...shown];
}
