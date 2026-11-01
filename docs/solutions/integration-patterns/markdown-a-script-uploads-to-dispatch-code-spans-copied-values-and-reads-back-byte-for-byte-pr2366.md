---
title: "Markdown a script uploads to Dispatch code-spans every copied value, and reads back byte for byte"
category: integration-patterns
tags:
  - dispatch
  - markdown
  - artifacts
  - reproducibility
date: 2026-10-28
status: active
module: scripts/dispatch
related_issues:
  - "sjawhar/legion#2366"
---

# Markdown a script uploads to Dispatch code-spans every copied value, and reads back byte for byte

When a script writes a Markdown document that is uploaded to Dispatch as a record someone must be
able to reproduce, follow these rules:

- **Put every value copied from the input in a code span.** That covers e-mail addresses, URLs,
  paths, owners and targets. Dispatch's document editor turns a bare address into a link when it
  stores the document, so the stored text differs from the script's output. Pick a fence of
  backticks the value does not contain, pad it with a space when the value starts or ends with a
  backtick, and in a table cell write `|` as `\|`. A plain hex or uuid id can stay bare.
- **Prove the round trip.** After the upload, read the document back with
  `dispatch doc-read --issue <KEY> --artifact <slug>`. Drop the read's trailing blank line and
  `Document token:` line, and compare the rest with the script's output byte for byte.
- **Keep the document under Dispatch's element limit.** A Markdown upload is refused past 65,536
  elements, and a table cell weighs 4. Move per-row data of more than about a thousand rows to a
  TSV file uploaded beside the document, and say in the document where the rows are.
- **Changing text the script itself writes changes the document.** Upload the new output as a new
  version, and keep the old version as the earlier record. Do not edit the uploaded document by
  hand.

## Evidence

The October comment-delivery report in sjawhar/legion#2366 (`scripts/dispatch/comment-delivery/`)
was refused at its first upload: the per-pair table passed the element limit at line 1551. The
second upload stored each bare author address as a link to itself, `[a@b.test](a@b.test)`. Commits
2381a30b (pairs to a TSV) and 86599d45 (`code()` and `cellCode()` in `render.ts`) made the read-back
equal to the output.
