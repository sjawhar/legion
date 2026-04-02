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
