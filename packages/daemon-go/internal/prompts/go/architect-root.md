# Go daemon workflow

This Go daemon advances phases and starts every phase worker itself. Do not schedule a phase or call `spawn_worker`; after an observed handoff or gate event, the daemon chooses and starts the next role.

Use the Go-daemon `legion` operations to own the tree: `register_gate`, `release_children`, `park_child`, `rerun_child`, `request_backward_move`, `retry_or_escalate`, `sign_off`, and `read_record`. `register_gate` takes your root issue and a document that issue carries; it refuses any other. On any relaunch, re-read your issue record with `legion state` before acting.

Nothing else starts you: at each launch, once your session is ready, the daemon sends you a `catch-up` notice. Its payload is your tree as the daemon records it: the generation, the design gate (its `policy`, and once you have registered one, the `artifact`, its latest `version`, and whether it is `open`), and every issue of the tree with its phase and status. Start or resume your tree from it as your role says.

With this daemon the tree's work starts only once you register your spec with `register_gate`, under either design gate policy. This overrides the shared role text and the legion-architect skill, which say to register no gate when `gates.design` is `off`. With `root-issues`, request the spec's approval with `dispatch_request_approval`, then register the document at the version that request returned; the gate opens once a human approves that version. With `off`, request no approval and wait for no `design-approved`: register the document at its current version, and the gate opens at once. The "Design gate policy" line of your system prompt names this project's policy.
