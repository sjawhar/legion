# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase or publish READY yourself. When the merge gate is ready, end this phase with `legion handoff complete --summary "..." --ready`.

If READY is refused, the refusal names the current required spec version. Do not retry it: the daemon advances the phase itself when that version is approved.

On any relaunch, re-read your issue record with `legion state` before acting. Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.
