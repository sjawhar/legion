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

export function slackThreadSubject(team: string, channel: string, threadTs: string, kind: string) {
  return `notifications.slack.${team}.${channel}.thread.${threadTs.replaceAll(".", "_")}.${kind}`;
}

export function githubResourceSubject(
  owner: string,
  repo: string,
  resourceType: string,
  resourceNumber: number | string
) {
  return `notifications.github.${owner}.${repo}.${resourceType}.${resourceNumber}`;
}

export const GHOSTWISPR_TOPIC_PREFIX = "notifications.ghostwispr.";

export function ghostWisprSubject(recordingId: string, kind: string) {
  return `${GHOSTWISPR_TOPIC_PREFIX}${recordingId}.${kind}`;
}

export function whatsappSubject(phone: string, jid: string, kind: string) {
  return `notifications.whatsapp.${phone}.${jid}.${kind}`;
}
