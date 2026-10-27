# How a role claim lives

Read this when a role you held is gone after a resume, fork or restart, when `envoy_role_set` is refused, or when a role publish returns `no holder`.

- A claim belongs to the session, not the process. `omp --resume` re-claims the role recorded in the session transcript, also after the listener removed the dead session's old row (a session's registration lapses 5 minutes after its last heartbeat).
- `/fork`, `/branch`, `/handoff` and any other switch that carries the transcript read the role the previous session id held and move it to the new id while the new session starts. `/new` and `/resume` install an unrelated transcript and carry nothing.
- A process that dies and is never resumed stops being a live holder. Once its registration lapses, role publishes return `no holder` until that transcript resumes or another session claims the role.
- An automatic reclaim never steals. It is a soft claim, which the listener grants only when the role is unheld, held by a session that is no longer live, or held by the id this session continues (a fork's parent). When a different live session holds it (the parent of a `/fork` whose role moved to the child, or a second process on the same transcript), the listener refuses atomically, and the resumed session logs a warning and holds nothing.
- Only an explicit `envoy_role_set` is last-claim-wins. Running it again for a role this session already holds is harmless.
- If another plugin made subscriptions that depend on your current role, `envoy_role_set` to a different role is refused, and the refusal names those subscriptions. `envoy_unsubscribe` keeps those subscriptions and lists them as kept.
