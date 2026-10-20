---
title: "A DOM-measuring hook's two stale views: the commit a runtime owns, and the node a boundary replaced"
category: dispatch-web
tags:
  - react-hooks
  - uselayouteffect
  - mutationobserver
  - error-boundary
  - assistant-ui
date: 2026-10-20
status: active
module: packages/dispatch/web/src/features
related_issues:
  - "INFRA-43"
  - "sjawhar/legion#2084"
---

# A DOM-measuring hook's two stale views: the commit a runtime owns, and the node a boundary replaced

A hook that measures the DOM is wrong in two ways that type-check, render and pass a unit suite.

**The commit.** A third-party runtime that applies state in a *passive* effect renders its content
one commit after the render that carried your props, and that commit need not render your hook at
all. Your `useLayoutEffect` therefore measures the previous DOM, and a compensation computed there
is worth zero pixels.

- Do not measure on the props. Compare what is **rendered** with what is **intended** — read the
  keys out of the DOM and compare them with the list you were given — and act only when they
  agree.
- When they disagree, arm the work and settle it from a `MutationObserver` on the subtree, in the
  mutation that lands the content. Connect the observer only inside that window; a transcript that
  streams would otherwise call it continuously.
- A unit row drives the immediate path, where the DOM and the props agree in one commit. Only a
  browser row that makes the runtime do the real thing (here: a real reconnect) drives the
  deferred path, which is the production one. Write that row, or the branch is unlocked.

**The node.** A hook that attaches listeners or observers must take the element
(`HTMLElement | null`) and carry it in every dependency list. A `RefObject` has a stable identity,
so effects that depend on it run on mount alone and keep their listeners on a detached node
forever once the element is replaced.

- Take the element through a callback ref into state (`ref={setViewport}`), not `useRef`.
- A replaced node arrives with none of the old one's scroll position. Re-baseline it in a layout
  effect keyed on its identity, before any listener can read that `scrollTop` of 0 as a reader
  gesture.
- The replacement is not hypothetical wherever a component renders inside an `ErrorBoundary` with
  a `resetKey`: the boundary renders a fallback while it holds an error and fresh children when
  the reset clears it.

## Evidence

In the agent live view (`features/agent-view/`), assistant-ui's `useExternalStoreRuntime` applies
a rebuilt transcript in a passive effect. The first implementation compensated the reader's turn
in the layout effect that saw the new ids: measured in Chromium, it moved the reader by nothing
(`before {id:"m12",top:-8}` → `after {id:"m20",top:-8}`, the eight dropped turns), and what held
them was the browser's own anchoring. The fix compares the rendered `[data-message-id]` set with
`contentIds` and, when they differ, settles from a `MutationObserver` armed for that window.

The same hook bound its scroll listener and both observers to `viewport.current` with stable
deps. With the node replaced under the mounted hook, a scroll back on the new node left
`atEnd true position 100`: no control, the hook believing it followed, and the next arrival
yanking the reader to the bottom — the original bug, reintroduced where nothing watched. Both
halves (the element in the deps, and the element-keyed re-baseline) are pinned by unit rows, and
must stay: no stream input reaches that boundary today, so no browser row can reproduce it.
