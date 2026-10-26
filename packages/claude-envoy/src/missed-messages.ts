// What a channel session shows for the messages it missed while its NATS subscriptions were down
// (`@legion/envoy-client/replay`): a replayed frame, a frame it could not be shown, and the notices
// that say replay could not keep its promise. The wording follows pi-envoy's cards.
import type { ReplayFailureNotice, ReplayNotice } from "@legion/envoy-client/replay"

/** A replayed frame: the frame as the shared renderer renders it, then when it was published. */
export function replayedContent(content: string, publishedAt: string): string {
  return `${content}\nPublished at ${publishedAt} while this session's connection was down.`
}

/** Shown in place of a frame the session could not be shown. */
export function failureNoticeContent(notice: ReplayFailureNotice): string {
  const readWith =
    notice.readWith === "dispatch read"
      ? "Read it with the `dispatch read` command on the issue or ask it concerns."
      : "Read it with the envoy_inbox tool."
  return [
    "envoy:",
    "  notice: message not shown",
    `  event_id: ${notice.eventId}`,
    `  from: ${notice.sender}`,
    `  summary: ${notice.summary}`,
    `  published_at: ${notice.publishedAt}`,
    ...(notice.subject === undefined ? [] : [`  subject: ${notice.subject}`]),
    `  error: ${notice.error}`,
    `  detail: This session could not be shown this message, so it is not coming back on its own. ${readWith}`,
  ].join("\n")
}

/** Tells the session that replay may have missed messages. */
export function replayNoticeContent(notice: ReplayNotice): string {
  const kind = notice.kind === "stream_recreated" ? "stream recreated" : "messages may be missing"
  return ["envoy:", `  notice: ${kind}`, `  detail: ${notice.message}`].join("\n")
}
