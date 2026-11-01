import { messageFor } from "@legion/envoy-client/errors"
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings"
import { sendEnvoyMessage } from "../src/envoy-client"
import { parseSendArguments } from "../src/send-arguments"

try {
  const { targetSession, message } = parseSendArguments(process.argv.slice(2))
  // The listener and bearer every command resolves: the envoy.json `envoyConfigPath("command")`
  // chooses (the ENVOY_CONFIG_FILE a session's plugin hands its shell first), with ENVOY_URL and
  // ENVOY_TOKEN_FILE or ENVOY_TOKEN over its envoy section. A send opens no NATS connection.
  const envoy = sessionEnvoySettings(sessionConfigFile("command", process.env), process.env, {
    cwd: process.cwd(),
    nats: false,
  })
  if (!envoy.enabled) throw new Error(`Envoy is off: ${envoy.error ?? envoy.notice}`)
  await sendEnvoyMessage({ baseUrl: envoy.url, token: envoy.token, targetSession, message })
  process.stdout.write(`Sent Envoy message to ${targetSession}.\n`)
} catch (error) {
  process.stderr.write(`Claude Envoy send failed: ${messageFor(error)}\n`)
  process.exitCode = 1
}
