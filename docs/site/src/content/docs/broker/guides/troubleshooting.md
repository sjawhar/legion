---
title: Troubleshooting
description: Every message agent-secrets and Dispatch show when the broker refuses something, what causes it, and how to fix it.
sidebar:
  order: 14
---

Find the message you see. `agent-secrets` prints a broker refusal as `<message> (<CODE>)` and a
helper refusal as `<CODE>: <message>`; the [error reference](/legion/broker/reference/errors/),
generated from the code, lists every code.

## The session has no broker identity

| Message | Cause | Fix |
| --- | --- | --- |
| `AGENT_SECRETS_URL is required` (exit 2) | The process does not know the broker's address. | Set `AGENT_SECRETS_URL` to the broker's public URL. A command run by `agent-secrets NAME -- …` keeps it. |
| `no session identity: the key dir … has no key.pem … and no helper socket is set or at …` | The process is neither in a container with a key nor on a machine with a helper. | On a machine, run the agent under `agent-secrets register --exec`; in a container, its launcher must enroll it. |
| `NOT_A_SESSION: pid … is not inside a registered host session; a session root is started with agent-secrets register --exec -- <agent argv>` | The helper is running, but this process does not descend from a registered session. | Start the agent with `agent-secrets register --wait 10 --exec -- <agent>`. |
| `NOT_ENROLLED: this session is not enrolled with the broker yet` | The session registered, but the helper has not enrolled it yet: it was started without `--wait`, or the broker was unreachable. | Start sessions with `--wait 10`. If it persists, check the helper's log and that `AGENT_SECRETS_URL` reaches the broker. |
| `this machine is not logged in to the secrets broker; not an agent session (run: agent-secrets launcher login)` | The helper holds no machine credential: it restarted, or the credential expired. | [Log the machine in](/legion/broker/guides/log-a-machine-in/). |
| `agent-secrets: helper unreachable at …` | The helper is not running, or the socket path is wrong. | Start `agent-secrets-helper`, or point `AGENT_SECRETS_HELPER_SOCK` at its socket. |
| `agent-secrets register: AGENT_SECRETS_HELPER_SOCK is unset and no helper socket is at … (host sessions only; an agent box has a key dir instead)` (exit 2) | No helper listens on the default socket, `$XDG_RUNTIME_DIR/agent-secrets/helper.sock`, and `AGENT_SECRETS_HELPER_SOCK` names none. | Start `agent-secrets-helper`, or set `AGENT_SECRETS_HELPER_SOCK` to the socket it listens on. |
| `the launcher credential reached its expiry; run: agent-secrets launcher login`, or `the broker refused the launcher credential (expired or revoked, …); run: agent-secrets launcher login` (from `launcher login-status`) | The credential expired or was refused: past its lifetime, [revoked by its operator](/legion/broker/guides/revoke-a-session/#end-a-machines-login), a clock far off the broker's, or an `AGENT_SECRETS_URL` that is not exactly the broker's public URL. | Fix the clock or the URL if either is wrong, then log in again. |
| `PROOF_INVALID` | The broker refused the session's signature: the session ended (its lease lapsed, its process exited, its launcher unenrolled it, or its machine's login was revoked), the clock is off by more than `BROKER_PROOF_SKEW_SECONDS`, or `AGENT_SECRETS_URL` differs from the broker's public URL. | Start a new session (a box: a new key and enrollment, [run an agent in a container](/legion/broker/guides/run-an-agent-in-a-container/)); check the clock and the URL. |

## The request was refused

| Message | Cause | Fix |
| --- | --- | --- |
| `request … was denied` (exit 77) | The approver denied it, or one of the names is a service's secret, which only that service's sessions get. Nothing ran. | Ask the approver why, or ask for the names you may have. |
| `request … is still waiting for approval; nothing was run. Check it with: agent-secrets status …` (exit 75) | Nobody decided within `--wait` (30 minutes by default). | Ask the approver; check with `agent-secrets status`; rerun the command once it is granted, or with a longer `--wait`, a duration such as `1h`. |
| `invalid value "…" for flag -wait: parse error` (exit 2) | `--wait` takes a duration with its unit (`90s`, `5m`, `1h`), not a bare number. `register --wait` is the exception: it takes whole seconds. | Add the unit. |
| `request … was expired` or `request … was cancelled` | Nobody decided it before it [expired](/legion/broker/concepts/#approvals), or the session cancelled it (`agent-secrets cancel`). A session that ended reads none of its requests: its calls are refused `PROOF_INVALID`. | Ask again. |
| `no agent secret has this name (UNKNOWN_SECRET)` | No secret the broker serves has the name: none exists under the namespace in that name (`DEEL_API_KEY` is `<namespace>deel-api-key`), or the broker leaves it out because a tag is missing or malformed or it is on the wrong key, which the broker logs by name (`agent secret policy refused`, [Operating the broker](/legion/broker/operate/#health-and-logs)). | Check the spelling; ask the secret's owner, or whoever runs the broker, to fix its tags or key. |
| `the requested secrets need different approvers; request them separately (MIXED_APPROVERS)` | One command asked for secrets that different people approve. | Request them in separate commands. |
| `the grant released no value for …; nothing was run` | The broker granted a name but returned no value for it. | Tell whoever runs the broker. |
| `grant is expired, revoked, or its session ended (GRANT_NOT_LIVE)` | The grant ended between the decision and the read, or the secret's tags changed since it was granted: the secret is now left out or denied, one granted automatically now needs approval, or it now belongs to someone who did not approve the grant. | Run the command again to ask anew. |
| `secret is not in the secrets store (SECRET_NOT_IN_STORE)` | The secret was deleted from the secret store after the broker last read the namespace. | Ask again after the broker's next read, at most five minutes later; the request is then refused `UNKNOWN_SECRET` unless the secret is back. |
| `request object invalid (REQUEST_INVALID)` | The broker could not verify the signed request: usually a clock far off, or an `AGENT_SECRETS_URL` that is not the broker's public URL. | Check the clock and the URL. |

## Approving and logging in, in Dispatch

| Message | Cause | Fix |
| --- | --- | --- |
| `only the record's approver may decide it` (`NOT_APPROVER`) | You are not the approver the request names, or you approved it after the secret's owner changed while it waited ([what an owner change does](/legion/broker/concepts/#approvals)). | The named approver decides it. After an owner change, the new owner approves it if the request waited on anyone, and the approver it names can still deny it. |
| `request is already decided` or `this machine login has already been decided` (`RECORD_TERMINAL`) | It was decided already, or the session that asked has ended. | Nothing to do. |
| `… expired before its approver acted on it` (`RECORD_TERMINAL`) | It waited past its expiry: [a request's](/legion/broker/concepts/#approvals) or [a machine login's](/legion/broker/concepts/#machine-login). | The agent or the machine asks again. |
| `no pending machine login has this code` (`NO_SUCH_CODE`) | The code is mistyped, already decided, or expired. | Check the code on the machine's terminal; start a new login if it expired. |
| `confirmation code does not match` (`CODE_MISMATCH`) | The decision carried a different code from the login it names. | Look the code up again and decide from that page. |
| `this machine login's key already holds a live launcher credential` (`KEY_HOLDS_LIVE_CREDENTIAL`) | The machine signed two logins with one key, and one is already approved. | Deny this one; the machine is already logged in. |
| No **Credential requests** section and no **Live grants** | Dispatch has no broker connected (`DISPATCH_AGENT_SECRETS_URL` is unset). | See [Operating the broker](/legion/broker/operate/#what-it-depends-on). |
