---
title: "kubectl output names the namespace and an admission refusal names its binding: elide both before quoting"
category: legion
tags:
  - private-names
  - kubectl
  - evidence
date: 2026-10-25
status: active
module: legion-worker
related_issues:
  - "INFRA-386"
  - "sjawhar/legion#2196"
---

# kubectl output names the namespace and an admission refusal names its binding: elide both before quoting

Extends docs/solutions/legion/a-pod-name-copied-from-dispatch-into-a-pull-request-carries-the-deployments-project-key-and-fails-the-private-names-check-LEGION-164.md.

Before you quote kubectl output in a public pull request, a handoff or a message, pass it through
a filter that replaces each of these:

- **The namespace, which kubectl prints itself even when the command never named it:**
  `No resources found in <namespace> namespace.` and `job.batch "<name>" deleted from <namespace>
  namespace`.
- **Both names in an admission refusal:** `ValidatingAdmissionPolicy '<policy>' with binding
  '<binding>' denied request: …`. The binding name is easy to miss once the policy name is elided.
- **The refusal's rule message, when it holds a declared name as an ordinary word,** such as the
  compute class. Quote the rule by what it enforces ("naming the four-processor rule").

`check-private-names.sh` does not know a deployment's namespace or policy names. A sweep does:
read the literals into shell variables from the live sources (the projected credential's namespace
file, the deployment's declaration) and match short, generic ones as whole words.

## Evidence

sjawhar/legion#2196: every live run printed the namespace in its `No resources found` and
`deleted from` lines. The refused-create fixture printed the policy name, the binding name, and a
rule message containing the compute class's name as a word. All three were elided by `sed`
before any quote. The sweep, built that way, gave 0 hits on every diff, body and handoff, and 1 on
a scratch file holding one literal.
