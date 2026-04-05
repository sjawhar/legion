export const AGENT_TOPIC_PREFIX = "notifications.agent.";

export function agentSubject(session: string) {
  return `${AGENT_TOPIC_PREFIX}${session}`;
}

export function githubSubject(owner: string, repo: string, kind: string) {
  return `notifications.github.${owner}.${repo}.${kind}`;
}

export function slackSubject(team: string, channel: string, kind: string) {
  return `notifications.slack.${team}.${channel}.${kind}`;
}
