---
name: plain-words
description: "Use before you send or publish any text a person reads: an ask, a spec, a Dispatch comment, a message to a colleague, a pull-request body, a commit message, a report, a status update. Also use before you answer a question a person asked, and before a draft goes to a reviewer. It removes empty verb phrases and replaces each with the literal thing."
---

# Say the literal thing

The operator made this a rule: delete the empty verb phrases agents keep using, and say the literal
thing instead.

Run this over your text before you send it. It takes a minute and it is not optional.

## Answering a question

1. Answer the exact question, only it, in the order it was asked, because anything else reads as
   evasion and the question comes again.
2. Start from what happens, not from a setting, a flag or a name: the reader can reach the setting
   from the behaviour, not the reverse.
3. Define each term where you first use it, in literal words. No analogy: the reader cannot tell
   which part of one is true.

## Before you send

A fresh session that has only what the reader has removes what the reader does not need, and lists
every term it cannot define. Fix each listed term. A reviewer that reads only to approve has not
done this step, and an instruction alone did not make it happen, so the two lists are what show it
ran. The check runs on the text going to the reader, never on the instructions you give that
session or on what it sends back, which would never end.

## The test

Take the verb phrase out and ask: **what literally happens?** Write that. If you cannot say what
happens, you did not know, and the phrase hid it from you as well as from the reader.

## The phrases, commonest first

| Never | What to write |
|---|---|
| land the change, land the PR | merge it, release it, deploy it, apply it — whichever one it is |
| unblock the work | X can start once Y merges |
| name the problem, name the owner, name a date | say the problem; who owns it is X; it is due on <date> |
| it pays for itself, that pays | it costs $16,306 and returns $60,379 a month |
| carry it to X, carry the finding | tell X; send it to X; put it on the issue |
| this shows the gap, the number shows | the gap is 12 of 30; the number is 1.17 |

The rows are in the order agents write them, so look for the first row first: you reach for "land"
when you cannot say whether the thing merged, released or deployed. If you can say which, say it.
If you cannot, that is the sentence — "it is approved and not merged" — and it is more useful than
any verb.

On the "name the X" row an instruction alone does not work, so use the test: delete "name". If the
sentence then says what happens, keep it that way; if it says nothing, you had not decided the
action, so decide it and write it.

## Do not shout

No ALL-CAPS headings. No ALL-CAPS phrases for emphasis. Bold only where a number, a name or a
choice needs to be found again.

The reason it appears: an agent writes a long passage, cannot find its point, and puts a handle on
it so a reader can. **The handle is a symptom. Cut the passage instead.** A paragraph that needs a
shouted header to be skimmable is a paragraph nobody should have to skim.

The same goes for a message to another agent. A capitalised line in a brief reads as an instruction
shouted twice, and a reader who sees three of them stops seeing any.

## What stays

Plain verbs that describe a real action stay: merge, delete, send, read, measure, kill a process by
pid, repoint a link. The rule is against phrases that sound like an action and name none.

A term of art stays when it is the literal name of the thing: a merge queue merges, a garbage
collector collects, a sweep sweeps.

## Where this fits

`skill://asd-ste100` constrains grammar and vocabulary for a reader whose first language may not be
English. This skill removes verb phrases that carry no fact. Neither removes AI writing tells, so
where your session has a pass for those it is a third one. Run them all on an outside message
(`skill://outside-message`); run this one on everything.

The two passes themselves — the structural one and the one that removes the writing that marks
text as an agent's — are `skill://plain-words/references/mechanics.md`.

One kind of text may also have a form skill of its own — a comment to another project's
maintainers, a document a buyer reads — where the operator ships one; your stream's brief names
it. This skill holds for all text either way.
