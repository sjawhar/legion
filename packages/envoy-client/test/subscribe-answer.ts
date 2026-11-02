import { deliverSubject } from "../src/consumer-delivery";
import type { DeliveryAnswer } from "../src/transport";

/** The stream id a stand-in listener names when the test has no reason to pick one. */
export const TEST_STREAM_ID = "stream-1";

/**
 * The `delivery` a listener answers a registration it can give a consumer: the session's own
 * relay subject, which is the one path a message reaches it by (INFRA-771). Every stand-in
 * listener in this repository answers with this, so none of them drifts from what the client
 * accepts: a registration answered anything else — no `delivery` at all, the `core` mode this
 * release dropped, or a field the client cannot read — fails in `parseSubscribeAnswer`, and a
 * stand-in that answered one of those would leave its session deaf rather than test anything.
 *
 * `streamID` names the stream for a test whose assertions read it, and `gap` is set by a test
 * driving the gap notice; the defaults are what a test that cares about neither wants.
 */
export function consumerDelivery(
  sessionID: string,
  options: { readonly streamID?: string; readonly gap?: boolean } = {}
): DeliveryAnswer {
  return {
    mode: "consumer",
    subject: deliverSubject(sessionID),
    stream_id: options.streamID ?? TEST_STREAM_ID,
    gap: options.gap ?? false,
  };
}
