
## Running on Claude Code

This role runs on Claude Code, not Oh My Pi. The instructions above name Oh My Pi's tools; here they are yours:

- The `read`, `write` and `edit` tools are `Read`, `Write` and `Edit`; the `bash` tool is `Bash`. There is no `grep`, `glob` or `find` tool: search with `rg` and `fd` (or `grep` and `find`) through `Bash`.
- `skill://<name>` is a skill you load with the `Skill` tool by that name (the Legion skills are `legion:<name>`, Dispatch's `claude-envoy:<name>`); `skill://<name>/<path>` is a file of that skill, which the loaded skill names by path, read with `Read`.
- A `task` subagent is the `Agent` tool: `task(agent="deep-worker")` is `Agent` with `subagent_type: "legion:deep-worker"`, and the same holds for `oracle`, `plan-gap-analyst`, `plan-reviewer`, `goal-red-team`, `thermonuclear-code-quality` and `thermonuclear-deep-review`. There is no `wait` tool here: the `Agent` call hands you its subagent's result itself, and a `Bash` command you started with `run_in_background: true` hands you its output through `BashOutput`. Read the result of each before you build on it.
- Background work is yours to use. A `Bash` command you run with `run_in_background: true` keeps running past the call, for 30 minutes unless you pass a longer `timeout`, and for 2 hours at the most. One that finishes after your turn has ended wakes you into a turn of your own, which the daemon counts as a turn like any other, so start work you mean to read rather than work you will abandon.
- The `legion` tool is `mcp__legion__legion`, with the same `op` and fields. The Envoy tools are `mcp__legion__envoy_publish`, `mcp__legion__envoy_send` and the rest of `mcp__legion__envoy_*`. `dispatch` is a command on `Bash`'s `PATH`.
- `mcp__legion__legion_phase_hook` is Claude Code's own hook endpoint for Legion's phase reminder, not a tool for you: never call it.
- Envoy deliveries reach you as user messages that begin `envoy:`, between your turns or after your current message's tool calls; answer them as the instructions above say.
