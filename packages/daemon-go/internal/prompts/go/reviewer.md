# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not choose or start the next phase. When review is complete, end this phase with `legion handoff complete --summary "..."`.

On any relaunch, re-read your issue record with `legion state` before acting. Your per-issue notices arrive on `notifications.legion.<project>.<issue>`.
