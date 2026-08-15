export type SubscriptionTopicsOptions = {
  readonly sessionId: string
  readonly additionalTopics: string | undefined
}

export function subscriptionTopics(options: SubscriptionTopicsOptions): readonly string[] {
  const additional = options.additionalTopics
    ?.split(",")
    .map((topic) => topic.trim())
    .filter((topic) => topic.length > 0)

  return [`notifications.agent.${options.sessionId}`, ...(additional ?? [])]
}
