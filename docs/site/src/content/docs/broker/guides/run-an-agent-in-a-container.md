---
title: Run an agent in a container
description: Give an agent in a container (a box) its own signing key, enroll it through the machine's helper, keep its lease alive, and end it.
sidebar:
  order: 11.5
---

An agent in a container on a machine, a **box**, signs its broker calls with a key of its own
rather than asking the machine's helper to sign for it. The machine's helper still enrolls it,
under the machine's login, so the box holds nothing but its key and its enrollment id. This guide
needs a machine that is already [logged in](/legion/broker/guides/log-a-machine-in/), with its
helper running.

The box's key directory is shared: the box keeps its key there, and the enrollment on the machine
writes the enrollment id beside it. Mount a directory of the machine into the container (a tmpfs
is best, since the key never needs to outlive the box) and point `AGENT_SECRETS_KEY_DIR` at it on
both sides. The box also needs `AGENT_SECRETS_URL`, the broker's address.

## 1. Make the box's key, inside the box

```console
$ agent-secrets keygen --out "$AGENT_SECRETS_KEY_DIR"
8fa_6Q6_dZjxymPd1gyObzWQpCn06S3L9L2PqZzN5dM
```

`keygen` writes `key.pem` (readable by its owner alone) and prints the key's thumbprint.

## 2. Enroll it, on the machine

Name the box with a runtime id that stays the same for its whole life (its container id, say), and
pass the thumbprint:

```console
$ agent-secrets enroll --helper --kind box --runtime-id example-box-1 --thumbprint 8fa_6Q6_dZjxymPd1gyObzWQpCn06S3L9L2PqZzN5dM
edd9a18c-41ff-46f9-8a15-bddb30f0abc3
```

The helper enrolls the box under the machine's login, and `enroll` writes the enrollment id to
`$AGENT_SECRETS_KEY_DIR/enrollment`, where the box reads it. A call the box makes before then waits
up to `AGENT_SECRETS_ENROLL_WAIT` (20 seconds by default) for that file.

## 3. Keep its lease alive, inside the box

An enrollment is leased for `BROKER_LEASE_SECONDS` (15 minutes by default). Run
`agent-secrets renew` in the box for its whole life; it renews the lease until it is stopped:

```sh
agent-secrets renew &
```

The box's agent then uses secrets as any session does:

```console
$ agent-secrets self
enrollment_id: edd9a18c-41ff-46f9-8a15-bddb30f0abc3
kind: box
operator: ada@example.com
lease_expires_at: 2026-10-03T03:39:03Z
$ agent-secrets DEMO_READ_TOKEN -- printenv DEMO_READ_TOKEN
demo-read-token-value
```

## When the box stops

Unenroll it from the machine when the container exits; it prints nothing and exits 0, and the
broker revokes the box's grants and cancels its pending requests at once:

```sh
agent-secrets unenroll --helper --enrollment edd9a18c-41ff-46f9-8a15-bddb30f0abc3
```

A box nobody unenrolls ends anyway once `agent-secrets renew` stops. Its lease lapses
`BROKER_LEASE_SECONDS` after the last renewal, and from then on every call it makes is refused:

```console
$ agent-secrets self
agent-secrets self: proof invalid: not live (PROOF_INVALID)
```

On its next run, within `BROKER_SWEEP_SECONDS` (5 seconds by default), the broker's sweep ends the
enrollment: its grants are revoked, its pending requests are cancelled and leave the approver's
Inbox (approving one now answers `request is already decided`), and the broker logs:

```text
INFO broker sweeper: ended an enrollment whose lease lapsed enrollment_id=edd9a18c-41ff-46f9-8a15-bddb30f0abc3 kind=box runtime_id=example-box-1 slot="" lease_expired_at=2026-10-03T03:39:03.386Z grants_revoked=1 requests_cancelled=1
```

A lapsed lease cannot be renewed: to use the box again, start over from step 1 with a new key.
