---
name: legion-oracle
description: Use when you are about to ask a human a question about a pattern, convention, solved problem or technical approach that the repository or its docs/solutions/ might already answer.
---

# Legion Oracle

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

## When to research instead of asking

- Research when the answer is a fact a file, command or document can show (a pattern, convention, solved problem or technical approach), because `docs/solutions/` records what past work learned and a human should not answer it twice.
- Ask the human directly when the answer is a choice between options that each work (a preference, requirement, scope decision or judgment call), because no file can choose. Quote any earlier decision a file records (`file:line`), so the human can confirm it in one word.

## Research steps

If the deployment instructions (the `# Deployment instructions` part of your system prompt, from
`legion.yaml`'s `instructions:`) name a role and say it answers research questions (a role, not the
`oracle` task agent), send your question to it first with
`envoy_publish(topic="notifications.role.<name>", expects_reply="required")`, because that role may
already hold the answer. If the publish returns an error, research yourself and put the error in
your output. Otherwise wait up to 15 minutes for the reply, so a silent role cannot hold your work;
if none comes, research yourself and say that the role did not answer.

Run steps 1 and 2, in parallel if you choose, since neither needs the other's result. Run step 3
when the question names a library, and step 4 when it asks for general practice, each only when
steps 1 and 2 give no `file:line` source for some part of the question. Use only `read`, `grep`,
`web_search`, the `envoy_publish` above, and `task` with `scout` or `oracle`: the daemon's boot
gate checks that the agents Legion's prompts name exist, and no other.

| Step | Tool | Query |
|------|------|-------|
| 1. Institutional learnings | `grep`, then `read`, over `docs/solutions/` (front-matter `tags` first, then the body) | each name the question gives (module, file, function, error text), then two or three words for the problem |
| 2. Codebase patterns | `task(agent="scout")` for a read-only search; `task(agent="oracle")` when scout's answer spans two or more modules or shows patterns that disagree | how [module] handles [topic] |
| 3. Framework docs | `web_search`, then `read` the library's official docs page | [library] docs [topic] |
| 4. External practices | `web_search`, then `read` what the tool's or standard's maintainer publishes (docs, RFC, changelog); give its URL | current practice for [topic] |

Before you rely on a step 1 hit, `grep` `docs/solutions/` for its path. If another file lists that path under its front-matter `supersedes:` key (on that line or as a list item below it), use that file instead; if one has a line `Extends docs/solutions/<hit path>`, read both. Nothing else reconciles these links.

## Output

- Found: the answer with its source (`file:line` or a URL), so the reader can check it.
- Partly found: each part found with its source, then each part not found.
- Not found: "Checked docs/solutions/ (queries: ...) and codebase (scout query: ...) - no file answers this", so the reader knows what was searched. Then ask the human the part no step answered.
