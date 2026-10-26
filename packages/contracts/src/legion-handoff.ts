import { z } from "zod";
import { EnvelopeSchema } from "./envelope";
import { LEGION_NOTICE_TOPIC_PREFIX } from "./subject";

/**
 * The subject the Envoy listener hands a stopped Legion agent's message off to its daemon on:
 * `notifications.legion.<project>.agents.<session>`, `project` the Legion project token role
 * tokens carry (not the Dispatch project key) and `session` the agent session the message is for.
 * The notification stream retains `notifications.legion.>`, so the daemon's durable consumer on
 * `notifications.legion.<project>.agents.>` reads every hand-off, one published while it was down
 * included. Mirrored in Go as `intake.HandoffSubject` (packages/daemon/internal/intake/handoff.go).
 */
export function legionAgentHandoffSubject(project: string, session: string): string {
  return `${LEGION_NOTICE_TOPIC_PREFIX}${project}.agents.${session}`;
}

/**
 * One message the listener hands off to the Legion daemon in place of refusing or dropping it,
 * because it is for an agent that is not running and whose daemon answers for it (the Legion
 * agent registry names the session, and its project's `answering` heartbeat is fresh):
 *
 * - `original_topic` is the topic the message was for: the agent's own
 *   (`notifications.agent.<session>`) or its role's (`notifications.role.<role token>`).
 * - `target_session` is the agent session it is for: the session addressed, or the role's newest.
 * - `role_token` is that session's Legion role token, `project` its Legion project token.
 * - `envelope` is the Envoy envelope exactly as it would have been delivered to the agent: a
 *   Dispatch delivery's envelope keeps its `payload`, the JSON Dispatch put there (a targeted
 *   send's `{event, delivery}` frame, or a bare event on a follower or author route), whose event
 *   carries its `type`, `actor`, the message, comment or ask id, the issue key and `reply_to`.
 *
 * The Go daemon decodes it as `intake.Handoff` (packages/daemon/internal/intake/handoff.go).
 */
export const LegionAgentHandoffSchema = z
  .object({
    original_topic: z.string().min(1),
    target_session: z.string().min(1),
    role_token: z.string().min(1),
    project: z.string().min(1),
    envelope: EnvelopeSchema,
  })
  .strict();

export type LegionAgentHandoff = z.infer<typeof LegionAgentHandoffSchema>;
