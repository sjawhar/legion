---
title: "Inline HTML Dashboard Pattern: Self-Contained UI in TypeScript Template Literals"
category: daemon
tags:
  - dashboard
  - html
  - template-literal
  - zero-dependency
  - mvp
  - server
  - http
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "515"
symptoms:
  - "need to serve a web UI from the daemon without a build step"
  - "how to embed HTML/CSS/JS in a TypeScript file"
  - "dashboard served from daemon HTTP server"
---

# Inline HTML Dashboard Pattern

## Context

Issue #515 added a web dashboard UI to the Legion daemon. The approach: a single
TypeScript function (`getDashboardHtml()`) returns a complete HTML document as a
template literal string. The daemon serves it at `GET /dashboard/ui` with a 6-line
route handler.

## The Pattern

### Architecture

```
GET /dashboard/ui  →  getDashboardHtml()  →  static HTML shell
                                              ↓ (client-side)
                                         fetch("/dashboard")  →  JSON API (already existed)
                                              ↓
                                         vanilla JS renders DOM
```

Key separation: the HTML is a **static shell** with client-side JS that fetches data
from the existing JSON endpoint. No server-side rendering, no data baked into the HTML.
This means the JSON API and the UI are independently testable and evolvable.

### Server Integration

The entire integration in `server.ts` is:

```typescript
import { getDashboardHtml } from "./dashboard-ui";

// In the route handler:
if (method === "GET" && url.pathname === "/dashboard/ui") {
  return new Response(getDashboardHtml(), {
    status: 200,
    headers: { "content-type": "text/html; charset=utf-8" },
  });
}
```

The `getDashboardHtml()` function has **zero imports and zero parameters** — it's a
pure function that always returns the same string.

### Template Literal Escaping Gotchas

The entire HTML/CSS/JS document lives inside a JavaScript template literal. This
creates escaping challenges:

1. **Client-side JS must avoid template literals.** The implementer used string
   concatenation (`+`) and `.forEach` callbacks instead of template literals and
   arrow functions. This sidesteps the escaping problem entirely — backticks inside
   the outer template literal would terminate it.

2. **CSS Unicode escapes need double backslash.** `content: "\25B6"` in CSS becomes
   `content: "\\25B6"` inside the template literal. The backslash is consumed once
   by JS string parsing.

3. **Regex backslashes need double escaping.** `/\s+/g` in the client JS becomes
   `/\\s+/g` inside the template literal. Easy to miss — the regex works fine in a
   standalone `.js` file but silently breaks inside a template literal with single
   backslash.

4. **No `${` sequences in client JS.** Template literal interpolation markers in the
   client code would be evaluated at build time, not runtime. The implementation
   avoids this by not using template literals on the client side.

### When This Pattern Works

- **MVP dashboards** — under ~500 lines total, read-only, polling-based
- **Admin/debug UIs** — status pages, health dashboards, log viewers
- **Single-page tools** — no routing, no authentication, no complex state
- **Zero-dependency constraint** — when adding a bundler or framework would be overkill

### When to Migrate Away

- **Interactive controls** — forms, approve/reject buttons, inline editing. The
  `innerHTML`-based re-render destroys form state on every cycle.
- **Multiple pages/views** — no client-side routing, URL state management, or back
  button support.
- **Growing complexity** — once the file exceeds ~500 lines, the lack of IDE
  support for HTML/CSS/JS inside a template literal becomes a significant drag.
  No syntax highlighting, no autocomplete, no linting.
- **Real-time updates** — polling works for dashboard refresh but WebSocket/SSE
  would be better for interactive use. The daemon already has Envoy for event
  routing.

## Trade-offs Made

| Decision | Benefit | Cost |
|----------|---------|------|
| Inline HTML in TS | Zero build step, zero asset pipeline | No IDE support for embedded HTML/CSS/JS |
| Client-side fetch | Reuses existing JSON API, cacheable HTML | Two HTTP requests on load, blank on JS failure |
| ES5-compatible JS | No transpilation needed | Verbose syntax (`.forEach`, `var`, no arrow functions) |
| 30s polling | Simple, works behind any proxy | Up to 30s stale, wastes bandwidth when idle |
| Minimal tests (2) | Low maintenance, tests the integration seam | No coverage of client-side rendering logic |

## Testing Approach

The tests verify the **integration seam** only:

1. Route returns `200` with `text/html; charset=utf-8` content type
2. Response contains key markers (`<!DOCTYPE html>`, `Legion Dashboard`, `fetch("/dashboard")`)

This is sufficient because:
- The HTML is static (no server-side data to verify)
- Client-side rendering correctness depends on the browser, not the server
- The JSON API (`/dashboard`) has its own test coverage

## Key Insight

The real value of this approach is **clean API separation**. Because the UI is just a
consumer of an existing JSON endpoint, it can be replaced later with a proper frontend
build (React, Svelte, etc.) without changing any backend code. The `/dashboard` JSON
contract is the stable interface — the inline HTML is a disposable MVP.
