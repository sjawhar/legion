import { z } from "zod"

const EnvoyEnvelopeSchema = z.object({
  topic: z.string().min(1),
  source_session: z.string().min(1).optional(),
  payload_summary: z.string().min(1),
})

export function formatMonitorEvent(input: string): string {
  try {
    const parsed = EnvoyEnvelopeSchema.safeParse(JSON.parse(input))

    if (!parsed.success) {
      return `[envoy] ${input}`
    }

    const source = parsed.data.source_session ?? "external"
    const summary = parsed.data.payload_summary.replaceAll(/\s+/g, " ").trim()
    return `[envoy topic=${parsed.data.topic} source=${source}] ${summary}`
  } catch (error) {
    if (error instanceof SyntaxError) {
      return `[envoy] ${input}`
    }

    throw error
  }
}
