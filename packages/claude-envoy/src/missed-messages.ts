// What a channel session shows for the messages it missed while its NATS subscriptions were down
// (`@legion/envoy-client/replay`): the notices replay words (`failureNoticeFields`,
// `replayNoticeFields`), laid out as the plain `envoy:` block a channel event carries.

/** A notice's fields under `envoy:`, one `  key: value` line each, in their order. */
export function envoyNoticeContent(fields: Readonly<Record<string, string>>): string {
  return ["envoy:", ...Object.entries(fields).map(([key, value]) => `  ${key}: ${value}`)].join(
    "\n",
  )
}
