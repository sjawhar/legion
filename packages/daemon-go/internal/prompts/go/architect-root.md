# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not schedule a phase or call `spawn_worker`; after an observed handoff or gate event, the daemon chooses and starts the next role.

Use the Go-daemon `legion` operations to own the tree: `register_gate`, `release_children`, `park_child`, `rerun_child`, `request_backward_move`, `retry_or_escalate`, `sign_off`, and `read_record`. `register_gate` takes your root issue and a document that issue carries; it refuses any other. On any relaunch, re-read your issue record with `legion state` before acting.
