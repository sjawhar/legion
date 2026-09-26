// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
/**
 * Preserves a `dispatch://` link target through Milkdown's href sanitizer.
 *
 * The commonmark preset's link mark renders `href` through `sanitizeLinkHref`, which
 * allows only http/https/mailto/tel/ftp, so a Markdown link to a Dispatch object —
 * `[spec](dispatch://CORE-1/spec)` — reaches the DOM as `<a href="">`. The sanitizer is
 * the right default for a document a stranger can edit, so it stays; the target is
 * carried alongside it as `data-dispatch-href`, and the host rewrites the anchor to its
 * own route from that attribute. Any other scheme is left to the sanitizer alone.
 */

import { linkAttr } from '@milkdown/preset-commonmark';
import type { Ctx } from '@milkdown/kit/ctx';

const DISPATCH_HREF = /^\s*dispatch:\/\//i;

export function dispatchHrefAttribute(href: unknown): Record<string, string> {
  if (typeof href !== 'string' || !DISPATCH_HREF.test(href)) return {};
  return { 'data-dispatch-href': href.trim() };
}

/** Configures the commonmark link mark's extra DOM attributes; call inside `.config()` (browser)
 *  or against the headless ctx before the schema plugins run. */
export function configureDispatchLinks(ctx: Ctx): void {
  ctx.set(linkAttr.key, (mark) => dispatchHrefAttribute(mark.attrs.href));
}
