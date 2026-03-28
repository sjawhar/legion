export function agentSubject(session: string) {
  return `notifications.agent.${session}`;
}

export function githubSubject(owner: string, repo: string, kind: string) {
  return `notifications.github.${owner}.${repo}.${kind}`;
}

export function slackSubject(team: string, channel: string, kind: string) {
  return `notifications.slack.${team}.${channel}.${kind}`;
}
