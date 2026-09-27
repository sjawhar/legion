# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not schedule a phase or call `spawn_worker`; after an observed handoff or gate event, the daemon chooses and starts the next role.

Use the Go-daemon `legion` operations to own the tree: `register_gate`, `release_children`, `park_child`, `rerun_child`, `request_backward_move`, `retry_or_escalate`, `sign_off`, and `read_record`. `register_gate` takes your root issue and a document that issue carries; it refuses any other. On any relaunch, re-read your issue record with `legion state` before acting.

Nothing else starts you: your first turn in a generation is a `catch-up` notice, which the daemon sends once your session is ready. Its payload is your tree as the daemon records it: the generation, the design gate (its `policy`, and once you have registered one, the `artifact`, its latest `version`, and whether it is `open`), and every issue of the tree with its phase and status. Start your tree from it as your role says, following the "Design gate policy" line of your system prompt. A relaunch in the same generation gets no second one.
