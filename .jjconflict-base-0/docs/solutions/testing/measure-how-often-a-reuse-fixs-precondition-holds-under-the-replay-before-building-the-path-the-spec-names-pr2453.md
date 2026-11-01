---
title: "Measure how often a reuse fix's precondition holds under the replay before building the path the spec names"
category: testing
tags:
  - load-test
  - cache
  - measurement
  - planning
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2453"
---

# Measure how often a reuse fix's precondition holds under the replay before building the path the spec names

Before you build a cache or reuse fix:

- Find the state its precondition needs (a resident room, a warm replica, an earlier result in the same transaction). Read what removes that state under the load, such as idle eviction, reloads or a process restart.
- Read the base profile's split between the paths, for example the cold fork against the warm fork. When the path the spec names covers only the rare case, design a second exact path for the common case, and record it as a spec departure.
- Add a counter per path, and run one replay with a throwaway build that logs the counters, so the pull request states each path's share.

## Evidence

- sjawhar/legion#2453: rooms are evicted one minute after their last use (`roomIdleTimeout`, `service.go`), and under the replay most writes fork cold (the ranking's cold fork 1.75% against warm fork 0.58%). Reusing the write's own rendering (the spec's mechanism) classified 34 of the 248 observer updates. The added path, an update the room's load already held, classified 214. None rendered.
- The plan's spec departure 1 (plan handoff, `specDepartures`) records that path, and the pull request's E2E line gives the counts.
