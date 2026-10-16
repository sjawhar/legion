/**
 * The longest reply, in UTF-16 units, the `slack_post` tool posts with Slack's `chat.postMessage`.
 * Slack advises at most 4,000 characters in a message's text and cuts it past 40,000, so a longer
 * reply is refused, naming its length, rather than posted cut.
 */
export const SLACK_POST_TEXT_MAX = 4000;

/** A Slack channel id as a project's `slack_channels` names one: C for a public channel, G for a
 * private one, then capital letters and digits. It is one NATS subject token of the channel's
 * mention topic, so nothing else is taken. */
export const SLACK_CHANNEL_ID_PATTERN = /^[CG][A-Z0-9]+$/;

/** The Envoy topic of every app mention in `channel`, whatever the Slack team: the channel copy
 * Envoy's Slack webhook publishes (`notifications.slack.<team>.<channel>.mention`). */
export function slackMentionTopic(channel: string): string {
  return `notifications.slack.*.${channel}.mention`;
}

/** The channel token of an Envoy Slack topic (`notifications.slack.<team>.<channel>…`): `""` for a
 * Slack topic that names none, such as `notifications.slack.>`, and undefined for any topic that is
 * not Slack's. */
export function slackTopicChannel(topic: string): string | undefined {
  const [scope, source, , channel] = topic.split(".");
  return scope === "notifications" && source === "slack" ? (channel ?? "") : undefined;
}
