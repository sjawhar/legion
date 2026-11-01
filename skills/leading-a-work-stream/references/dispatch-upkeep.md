# Dispatch upkeep: the rest of the lead's rules

These go with the "Keep every request and wait in Dispatch" section of the skill.

- **Check a delegated or relayed result against its done before you report it**, because an agent's "done" is often a message sent, not a thing delivered. Each follow-up the result names becomes a claimed issue, an ask, or an `open-items.md` line, the same hour.
- **A step you keep back from a hand-over becomes its own claimed issue,** blocked by the handed-over one, or it is lost when the hand-over closes.
- **An owner whose session reads `unresponsive` has not dropped its work**: it is past the live window and inside the gone horizon, it keeps its issue claim and its role claim, and the message you send it waits for it. Leave its items with it, start no replacement, and give the work another owner only once its session reads `gone`, or has read `unresponsive` for more than 20 minutes.
- **After a restart or a compaction, first re-read your claimed issues and your open asks** (`dispatch open-asks`), then the transcript since your last audit, because what you were waiting for is not in the new context.
