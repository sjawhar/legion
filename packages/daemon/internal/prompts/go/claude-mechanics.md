
## Running on Claude Code

This role runs on Claude Code, not Oh My Pi. The instructions above name Oh My Pi's tools; here they are yours:

- The `read`, `write` and `edit` tools are `Read`, `Write` and `Edit`; the `bash` tool is `Bash`. There is no `grep`, `glob` or `find` tool: search with `rg` and `fd` (or `grep` and `find`) through `Bash`.
- `skill://<name>` is a skill you load with the `Skill` tool by that name (the Legion skills are `legion:<name>`, Dispatch's `claude-envoy:<name>`); `skill://<name>/<path>` is a file of that skill, which the loaded skill names by path, read with `Read`.
- A `task` subagent is the `Agent` tool: `task(agent="deep-worker")` is `Agent` with `subagent_type: "legion:deep-worker"`, and the same holds for `oracle`, `plan-gap-analyst`, `plan-reviewer`, `thermonuclear-code-quality` and `thermonuclear-deep-review`. Wait for each subagent's result; run none in the background.
- The `legion` tool is `mcp__legion__legion`, with the same `op` and fields. The Envoy tools are `mcp__legion__envoy_publish`, `mcp__legion__envoy_send` and the rest of `mcp__legion__envoy_*`. `dispatch` is a command on `Bash`'s `PATH`.
- Envoy deliveries reach you as user messages that begin `envoy:`, between your turns or after your current message's tool calls; answer them as the instructions above say.
