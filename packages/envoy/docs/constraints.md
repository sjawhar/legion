# Constraints

## Hard rules

- No more AWS changes from envoy work without explicit approval
- All envoy services run in Docker containers
- Do not install anything on hosts except Docker on `ghost-wispr`
- Do not disturb the Ghost Whisper app on the Raspberry Pi
- Do not commit, print, or push secrets
- Use `secrets ... -- command` for runtime secret injection

## Multi-repo hygiene

- Primary implementation lives in `~/legion/default/packages/envoy`
- If opencode or another maintained repo must change, that work stays isolated in that repo
- Never bundle those changes into an existing `sami` octopus merge
- Track every external-repo requirement in `docs/external-repos.md`

## Deployment posture

- Use Tailscale IPs for inter-machine traffic
- Use Docker on all four machines
- Prefer Compose-managed services and explicit mounted config
- Keep ports and public ingress limited to the already-open webhook receiver ports

## Source integration model

- Envoy has two ingestion paths: dedicated webhook receivers (`cmd/github/`, `cmd/slack/`, `cmd/ghostwispr/`) and the generic MCP bridge (`cmd/mcp/`). Both are valid patterns.
- The MCP bridge is the default path for new event sources — it connects to any MCP server that publishes resources, keeping source-specific logic out of Envoy.
- Building a dedicated receiver adds maintenance burden. It's justified for well-known webhook standards (GitHub, Slack) and simple webhook sources (Ghost Wispr), but consider whether the maintenance cost justifies the benefit over the generic MCP bridge before adding more.
- When using the MCP bridge, Envoy should stay naive about message content. The MCP server owns domain-specific logic (parsing, filtering, formatting); Envoy normalizes into envelopes and transports.
