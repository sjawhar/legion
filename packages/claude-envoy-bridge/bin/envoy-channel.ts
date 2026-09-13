import { runEnvoyChannelServer } from "../src/envoy-channel-server"

try {
  await runEnvoyChannelServer()
} catch (error) {
  const message = error instanceof Error ? error.message : "unknown channel server failure"
  process.stderr.write(`Claude Envoy channel server failed: ${message}\n`)
  process.exitCode = 1
}
