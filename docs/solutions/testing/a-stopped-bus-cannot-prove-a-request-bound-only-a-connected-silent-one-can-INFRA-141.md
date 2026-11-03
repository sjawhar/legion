---
title: "A stopped bus cannot prove a request bound; only a connected, silent one can"
category: testing
tags:
  - nats
  - deadline
  - frozen-relay
  - negative-control
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-141"
  - "sjawhar/legion#2167"
---

# A stopped bus cannot prove a request bound; only a connected, silent one can

- **To prove that a route answers inside its budget when the bus does not answer, keep the
  connection open and make it silent.** Put a relay between the client and the server and freeze
  it: it keeps both sockets open and moves no bytes. The client still reports itself connected.
- **Do not use a stopped server as that proof.** A stopped server closes the socket, and the client
  takes its disconnect path: it waits for a reconnect, and that wait honours the caller's context.
  So the route answers in time even when a call on it takes no context at all. The proof passes
  and proves nothing about those calls.
- **Run the proof per route.** Each route reaches its own calls into the client library. A
  bound proven on one route says nothing about the next. List every route the server registers,
  and freeze the bus under each one.
- **Bound the call that takes no context, at that call.** In nats.go, KV `Get`, `Create` and
  `Update` take no context and wait out the JetStream `MaxWait` (10 s by default) on a silent
  connection. Wrap each such call (`bus.Bounded`), not the route around it.

## Evidence

- The first proof of INFRA-141 stopped NATS under the running listener and got
  `HTTP 503 in 3.502745 s`: the reconnect wait honoured the deadline.
- With the bus connected but silent, the binary at e5185432 gave the send, the publish and the
  role publish no answer at 10 s. The sender stamp's read of the source session's interest record
  went through nats.go's KV `Get`, which takes no context (b5ab6896's message). Later rounds found
  more of the same, one route group at a time: the interest reads of `GET /v1/sessions`,
  `GET /v1/registry/<id>` and `GET /v1/interests/<id>` (dc06110d), the write routes (acafc88f), and
  the webhooks and `/healthz` (7093f375). The stopped-bus proof found none of them.
- The silent bus is `startListenerRelay`, armed (`arm`) to swallow what the client writes
  (`cmd/listener/reconnect_shutdown_test.go`), `newFrozenWebhookBus`
  (`cmd/listener/webhook_frozen_bus_test.go`), and `testnats.Server.Freeze`
  (`internal/testnats/nats.go`). `TestASendAndAPublishAnswerABusThatStoppedAnsweringInsideTheBudget`
  (`cmd/listener/send_route_acceptance_test.go`) uses the relay, with the same request asked before
  the freeze as its negative control.
- The pull request body's "Bounded paths" table lists each route and the bound that covers it.
