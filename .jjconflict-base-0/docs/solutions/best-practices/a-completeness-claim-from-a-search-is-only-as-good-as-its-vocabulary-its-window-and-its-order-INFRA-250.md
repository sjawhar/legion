---
title: "A completeness claim from a search is only as good as its vocabulary, its window and its order"
category: best-practices
tags:
  - documentation-drift
  - review-round
  - testing
  - search-method
  - nats
date: 2026-10-25
status: active
module: packages/envoy
applies_when:
  - You are about to write "every", "none", "no other" or "nothing else" on the strength of a grep
  - A review found one instance of a defect and you owe a sweep for the rest
  - You are classifying call sites by reading them in source order
related_issues:
  - "INFRA-250"
  - "sjawhar/legion#2189"
---

# A completeness claim from a search is only as good as its vocabulary, its window and its order

Extends docs/solutions/best-practices/a-sentence-that-enumerates-a-set-is-a-claim-about-every-member-check-the-far-side-of-the-boundary-INFRA-249.md.

That rule governs a sentence you write about a set. This one governs the **search you justify it
with**. A sweep has three independent ways to come back clean while the thing is present, and the
author is the worst judge of all three, because each is made of what they already thought of.

- **Vocabulary.** Search for the **post-condition**, not for the verbs. "A stream name that was
  deleted is present again inside one test" finds a restore, a snapshot replay, a helper and an
  alias; a list of create verbs finds only the verbs you remembered.
- **Window.** A search piped to `head` answers about the lines you printed, not about the tree. Count
  the hits before you conclude; `| wc -l` and the printed output must agree, or the claim is about a
  prefix.
- **Order.** Source order is not execution order. A delete inside `t.Cleanup` or `defer` sits
  **textually above** the create it follows and **temporally below** it. No pattern fixes this: the
  text simply is not the order, so every hit of a post-condition sweep is opened and read.

Write what the sweep covered, not that it was complete: *every site these patterns reach is X* is
checkable by the next person; *none is left unprotected* is not, and it is the sentence that stops
them looking.

## The sweep this came from, as a worked example

The question was whether any test in `packages/envoy` deletes a JetStream stream or bucket and puts
**the same name** back. That window matters because nats-server schedules a cleanup goroutine on
every stream delete (`stream.go:5279-5293` in v2.10.29) whose `os.Remove` of the account's streams
directory succeeds only when the account is now empty; a create landing inside that window fails
with `error creating store for stream`.

Pattern set: `DeleteStream`, `DeleteKeyValue`, `DeleteObjectStore`, `PurgeStream`, raw
`$JS.API.STREAM.DELETE` and `RESTORE` subjects, and **any** later appearance of the deleted name in
the same function, whatever the call is called. 23 delete sites; one was unprotected.

| Class | Delete sites | Where |
| --- | --- | --- |
| retried by `testnats.RecreateKeyValue` | 10 | `kvwatch/kvwatch_test.go:297,335,561,638,701,830`; `kvwatch/repair_test.go:114` through its callers at `:156`→`:159` and `:258`→`:260`; `store/interest_markers_test.go:303,416`; `store/kv_test.go:2003` |
| a fresh account per subtest, so the delete finds nothing | 1 | `bus/stream_config_test.go:453` |
| another stream stays in the account | 2 | `kvwatch/kvwatch_test.go:851` (the keeper at `:816`); `cmd/listener/main_test.go:4565`, recreated at `:4599` while the notification stream and role bucket stay |
| the name is never put back — the absence *is* the test's subject | 7 | `kvwatch/end_test.go:227`; `cistore/burst_test.go:940`; `store/kv_test.go:2041`; `cmd/listener/main_test.go:4463`; `store/interest_markers_test.go:460,480,532` |
| deleted only in `t.Cleanup`, so nothing follows it | 2 | `bus/kv_key_test.go:36,160` |
| the rig's own cleanup | 1 | `testnats/nats.go:227` |

Two ways to be safe, and they are not equal. `RecreateKeyValue` **survives** the window by retrying
`ErrCode 10049`. Keeping another stream in the account **removes** it: the cleanup still runs and
its `os.Remove` is a no-op while another stream's directory is there. Prefer removing the window;
when you do, give the extra stream a name and a comment saying why it exists, because it protects by
a side effect and the next tidier will delete an unexplained bucket.

Do not reach for a retry without reading which error it matches. A losing *restore* answers
`ErrCode 10062` (`restore failed: …`) on the end-of-data request, decoded as raw JSON rather than as
a `nats.APIError`, so `retryStreamStoreFailure`'s 10049 match would have caught nothing while the
sweep's sentence claimed the window was shut.

## Evidence

Three sweeps in one working day, each clean, each wrong, and no two for the same reason:

1. **Vocabulary, remembered name.** A sweep for a removed fallback searched `ENVOY_TEST_NATS_BIN`
   and `internal/testnats/local.go`, the names in the issue's spec. The fallback existed under
   `nats_backend=docker|process` with `E2E_NATS_SERVER`. The grep was right about its own words and
   false about the world.
2. **Vocabulary, unimagined spelling.** Round 7's sweep ran *two* pattern sets — the call shape
   (a delete followed by a create) and the server's own answer strings — and still missed
   `kvwatch_test.go`'s `restoreStream`, because a restore is a create under a name neither set
   contained and its call site carries no error string. The sweep that found it asked the
   post-condition instead.
3. **Window.** The follow-up claimed "no raw `$JS.API.STREAM.RESTORE` subject is sent anywhere, so a
   subject search could never have found this site". False: `kvwatch_test.go:921` sends
   `"$JS.API.STREAM.RESTORE." + stream` through `conn.Request`, and the grep's own pattern matched
   it — the command ended in `| head -12` and there were 31 hits. The conclusion was drawn from a
   prefix, and the explanation invented for it was a second false claim on top of the first.

The order trap was found while running the corrected sweep: `bus/kv_key_test.go:36` and `:160` read
as delete-then-create and are the exact reverse, because the delete is a `t.Cleanup`.

A reviewer re-ran the same sweep independently and found six factual errors in the first table —
a helper classified without reading its callers, a recreate missed at a caller's line, three rows
omitted so the counts summed to 20 rather than 23, and the two claims above. Every one was a
mis-read site rather than a missing pattern. **An independent re-run of the sweep is worth more than
a wider pattern**, because the second reader's vocabulary is not the first reader's.
