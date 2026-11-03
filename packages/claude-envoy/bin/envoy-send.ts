import { messageFor } from "@legion/envoy-client/errors"
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings"
import { configuredValue } from "../src/claude-session"
import { sendEnvoyMessage } from "../src/envoy-client"
import { parseSendArguments } from "../src/send-arguments"

try {
  const { targetSession, message } = parseSendArguments(process.argv.slice(2))
  // The listener and bearer every command resolves: the envoy.json `envoyConfigPath("command")`
  // chooses (the ENVOY_CONFIG_FILE a session's plugin hands its shell first), with ENVOY_URL and
  // ENVOY_TOKEN_FILE or ENVOY_TOKEN over its envoy section. A send opens no NATS connection.
  // The session this command acts as, where its shell names one: a broker trade writes under it.
  const session =
    configuredValue(process.env["ENVOY_SESSION_ID"]) ??
    configuredValue(process.env["CLAUDE_CODE_SESSION_ID"])
  const envoy = sessionEnvoySettings(sessionConfigFile("command", process.env), process.env, {
    cwd: process.cwd(),
    nats: false,
    ...(session === undefined ? {} : { session }),
  })
  if (!envoy.enabled) throw new Error(`Envoy is off: ${envoy.error ?? envoy.notice}`)
  await sendEnvoyMessage({
    baseUrl: envoy.url,
    credential: envoy.credential,
    targetSession,
    message,
  })
  process.stdout.write(`Sent Envoy message to ${targetSession}.\n`)
} catch (error) {
  process.stderr.write(`Claude Envoy send failed: ${messageFor(error)}\n`)
  process.exitCode = 1
}
