---
title: "A handler whose null clears refuses an absent field"
category: best-practices
tags:
  - wire-contracts
  - destructive-defaults
  - go
  - json
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/api/project_default_assignee.go
related_issues:
  - "INFRA-266"
  - "sjawhar/legion#2178"
---

# A handler whose null clears refuses an absent field

Extends docs/solutions/best-practices/a-wire-field-whose-empty-object-means-something-gets-a-presence-guard-not-the-blocks-spread-idiom-INFRA-188.md.

That document keeps the client from sending the destructive value by accident. This is the
server's half: the handler must not read the field's absence as the destructive meaning either.

- In Go, `*string` cannot tell `{}` from `{"field": null}`: both decode to `nil`. When `null`
  means "clear", decode the field as `json.RawMessage`. Empty means absent: refuse it with a 400
  naming the field. The literal `null` is the clear. Anything else is unmarshalled into the value.
- Look for the house idiom before writing one. `parseIssueAssignee` in `api/issue_assignee.go`
  returns `(value, provided, err)` for the same three-way read.
- Pin the refusal and the clear with separate tests. The clear's test stops a later reader from
  "fixing" the refusal by refusing every falsy body.

## Evidence

`PUT /api/v1/projects/{key}/default-assignee` with the body `{}` answered `200` and wiped the
project's stored default. The handler read an absent `assignee` (an empty `RawMessage`) as a
null. The contract, `ProjectDefaultAssigneeInput`, requires the field, and the sibling human PUTs
refuse `{}`. The tester's red test `TestPutProjectDefaultAssigneeRefusesABodyWithoutAssignee`
(0940abaf) pinned it. ca8c8b1c refuses `len(input.Assignee) == 0` with `400 INVALID_PROJECT`
before any lookup or write. `TestPutProjectDefaultAssigneeSetsChangesAndClears` keeps the explicit
null clearing.
