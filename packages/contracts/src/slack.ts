/**
 * The longest reply, in UTF-16 units, that the Envoy listener's `POST /v1/slack/post` sends to
 * Slack's `chat.postMessage`, and the `slack_post` tool's `text` maximum. Slack advises at most
 * 4,000 characters in a message's text and cuts it past 40,000, so a longer reply is refused,
 * naming its length, rather than posted cut. Generated into Go as `contracts.SlackPostTextMax`,
 * which the listener enforces, so the tool's refusal and the listener's cannot drift apart.
 */
export const SLACK_POST_TEXT_MAX = 4000;
