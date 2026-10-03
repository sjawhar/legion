---
title: Secrets Broker
description: Gives agent sessions short-lived access to secrets, with a human's approval where the rules ask for one.
sidebar:
  label: Introduction
  order: 0
---

The Secrets Broker gives agents short-lived access to secrets instead of standing credentials. An
agent session or pod enrolls with the broker under a key of its own and asks for a secret by name.
The broker's rules decide, per secret and requester, whether the request is granted automatically,
needs a named person's approval, or is refused. When it needs approval, the request appears in
that person's Dispatch Inbox to approve or deny. A grant expires on its own, and `agent-secrets`
runs a command with the granted value in that command's environment.
[How Legion, Dispatch and the broker fit together](/legion/how-it-fits/) shows where it sits.
