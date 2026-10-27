---
title: "go vet stops at the first type error: count broken call sites with go test -run NONE"
category: testing
tags:
  - go
  - go-vet
  - build-errors
  - scoping
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-379"
  - "sjawhar/legion#2194"
---

# go vet stops at the first type error: count broken call sites with go test -run NONE

- `go vet` reports one type error per package and stops. Never take the size of a fix from its
  output.
- To list every error, compile the tests and run none:
  `go test -count=1 -run NONE ./<pkg>`.
- Cross-check with a search for the changed symbol's call sites (`startBroker(`), across every
  build tag the package uses (`go vet -tags multiprocess` and the like).
- Quote the `go test` list in the issue or plan before you scope the work. Quote `go vet` only
  as the symptom.

## Evidence

On main `3ca092a69891`, `go vet ./...` in `packages/envoy` printed one error,
`service_account_scope_test.go:50:3: too many arguments in call to startBroker`. The INFRA-379
spec scoped the fix as that one call, two lines. `go test ./cmd/broker` on the same tree listed
four errors in two files (`service_account_scope_test.go:48:13`, `:50:3`;
`service_accounts_test.go:46:16`, `:48:3`). The planner found the second file with `go test`,
and the fix became two call sites (sjawhar/legion#2194).
