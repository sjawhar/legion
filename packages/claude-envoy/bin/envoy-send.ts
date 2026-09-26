import { sendEnvoyMessage } from "../src/envoy-client"
import { parseSendArguments } from "../src/send-arguments"

const ENVOY_URL = "http://127.0.0.1:9020"

try {
  const { targetSession, message } = parseSendArguments(process.argv.slice(2))
  await sendEnvoyMessage({
    baseUrl: process.env["ENVOY_URL"] ?? ENVOY_URL,
    targetSession,
    message,
  })
  process.stdout.write(`Sent Envoy message to ${targetSession}.\n`)
} catch (error) {
  const message = error instanceof Error ? error.message : "unknown send failure"
  process.stderr.write(`Claude Envoy send failed: ${message}\n`)
  process.exitCode = 1
}
