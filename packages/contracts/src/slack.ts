/**
 * The longest reply, in UTF-16 units, the `slack_post` tool posts with Slack's `chat.postMessage`.
 * Slack advises at most 4,000 characters in a message's text and cuts it past 40,000, so a longer
 * reply is refused, naming its length, rather than posted cut.
 */
export const SLACK_POST_TEXT_MAX = 4000;
