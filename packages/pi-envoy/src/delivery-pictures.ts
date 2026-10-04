import { readPictures } from "@legion/envoy-client/dispatch-execute";
import type { DispatchClient } from "@legion/envoy-client/dispatch-http";
import type { ContentBlock } from "./pi-types";

/**
 * The content an Inbox delivery hands the model: `text` alone, or, when it embeds pictures Dispatch
 * serves (`addresses`, in writing order), `text` followed by one image block per picture shown,
 * `PICTURES_SHOWN_MAX` and `PICTURES_SHOWN_MAX_BYTES` at most, as `dispatch_read` shows them.
 * `named` appends the `Pictures:` lines that say which image is which and name the rest; a
 * person's own turn keeps their text exactly as they sent it. A picture Dispatch cannot serve, or
 * no Dispatch configuration (`client` answers undefined), leaves the delivery its text.
 */
export async function withDeliveredPictures(
  text: string,
  addresses: readonly string[],
  client: () => DispatchClient | undefined,
  named: boolean
): Promise<string | ContentBlock[]> {
  if (addresses.length === 0) return text;
  const dispatch = client();
  if (dispatch === undefined) return text;
  const pictures = await readPictures(dispatch, [...new Set(addresses)]);
  if (pictures.images.length === 0) return text;
  return [
    { type: "text", text: named ? `${text}\n\n${pictures.lines.join("\n")}` : text },
    ...pictures.images.map(({ data, mimeType }) => ({ type: "image" as const, data, mimeType })),
  ];
}
