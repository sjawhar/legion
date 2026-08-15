import { runEnvoyMonitor } from "../src/envoy-monitor"

try {
  await runEnvoyMonitor()
} catch (error) {
  const message = error instanceof Error ? error.message : "unknown monitor failure"
  process.stderr.write(`Claude Envoy monitor failed: ${message}\n`)
  process.exitCode = 1
}
