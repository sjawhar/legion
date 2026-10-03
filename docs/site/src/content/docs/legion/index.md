---
title: Legion
description: The coordinator that runs coding agents on the Dispatch issues handed to it.
sidebar:
  label: Introduction
  order: 0
---

Legion is the coordinator that runs coding agents on the Dispatch issues handed to it. When a slot
is free it admits an issue, gives its tree an architect, and runs a planner, implementer, tester,
reviewer and merger on each change, each agent in its own workspace and each phase's handoff
committed to the issue branch. A human approves the spec at the design gate and merges the pull
request; Legion never merges. [How Legion, Dispatch and the broker fit together](/legion/how-it-fits/)
shows where it sits, and [Skills](/legion/legion/reference/skills/) lists the skills its agents load.
