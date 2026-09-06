// GitHub reference unfurling: after markdown rendering, `#N`, `owner/repo#N`,
// and bare GitHub issue/PR URLs become links whose text is the referenced
// title. Detection is pure (testable without a DOM); the DOM passes run in
// the browser after a region is painted.

export interface GitHubReference {
  readonly repo: string;
  readonly number: number;
}

export interface ReferenceMatch {
  readonly index: number;
  readonly length: number;
  readonly ref: GitHubReference;
}

// `owner/repo#N` first so the bare form does not claim its tail. A reference
// must start a word (start of text, whitespace, or an opening bracket) and
// end at a word boundary; `abc#12` and `word#3` are not references.
const REFERENCE_RE =
  /(^|[\s([])(?:([A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9_.-]+)#([1-9]\d*)|#([1-9]\d*))(?![\w#])/g;
const URL_RE =
  /^https?:\/\/github\.com\/([A-Za-z0-9-]+)\/([A-Za-z0-9_.-]+)\/(?:issues|pull)\/([1-9]\d*)(?:[#?].*)?$/;
// The `data-gh-ref` key an anchor carries. Anchors are not only the ones this
// module creates: the markdown sanitiser keeps `class` and `data-*`, so a
// comment body can plant `<a class="gh-ref" data-gh-ref="…">` with any value.
// Only a key in GitHub's owner/repo#N charset is trusted; anything else is
// neither fetched through the proxy nor put into a selector.
const REFERENCE_KEY_RE = /^[A-Za-z0-9-]+\/[A-Za-z0-9_.-]+#[1-9]\d*$/;

export function findReferences(text: string, threadRepo: string): ReferenceMatch[] {
  const matches: ReferenceMatch[] = [];
  for (const match of text.matchAll(REFERENCE_RE)) {
    const lead = match[1] ?? "";
    const repo = match[2] ?? threadRepo;
    const number = Number(match[3] ?? match[4]);
    matches.push({
      index: (match.index ?? 0) + lead.length,
      length: match[0].length - lead.length,
      ref: { repo, number },
    });
  }
  return matches;
}

export function referenceFromUrl(href: string): GitHubReference | null {
  const match = href.match(URL_RE);
  if (!match) return null;
  return { repo: `${match[1]}/${match[2]}`, number: Number(match[3]) };
}

function referenceKey(ref: GitHubReference): string {
  return `${ref.repo}#${ref.number}`;
}

/** The reference a `data-gh-ref` key names; null for a key outside the charset. */
export function parseReferenceKey(key: string): GitHubReference | null {
  if (!REFERENCE_KEY_RE.test(key)) return null;
  const [repo, number] = key.split("#") as [string, string];
  return { repo, number: Number(number) };
}

function referenceAnchor(doc: Document, ref: GitHubReference, text: string): HTMLAnchorElement {
  const anchor = doc.createElement("a");
  anchor.className = "gh-ref";
  anchor.dataset.ghRef = referenceKey(ref);
  anchor.href = `https://github.com/${ref.repo}/issues/${ref.number}`;
  anchor.target = "_blank";
  anchor.rel = "noreferrer";
  anchor.title = referenceKey(ref);
  anchor.textContent = text;
  return anchor;
}

const SKIP_ANCESTORS = "a, code, pre";

/**
 * Turn references in text nodes (outside links and code) into `a.gh-ref`
 * anchors with the plain reference as text, and mark bare GitHub issue/PR
 * links (text equal to the URL) the same way. Idempotent: anchors are never
 * re-linkified because they are skipped.
 */
export function linkifyReferences(root: ParentNode, threadRepo: string): void {
  const doc = root instanceof Document ? root : (root.ownerDocument ?? document);
  const walker = doc.createTreeWalker(root as Node, NodeFilter.SHOW_TEXT);
  const textNodes: Text[] = [];
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const text = node as Text;
    if (text.parentElement?.closest(SKIP_ANCESTORS)) continue;
    textNodes.push(text);
  }
  for (const text of textNodes) {
    const value = text.data;
    const matches = findReferences(value, threadRepo);
    if (matches.length === 0) continue;
    const fragment = doc.createDocumentFragment();
    let cursor = 0;
    for (const match of matches) {
      if (match.index > cursor) fragment.append(value.slice(cursor, match.index));
      fragment.append(
        referenceAnchor(doc, match.ref, value.slice(match.index, match.index + match.length))
      );
      cursor = match.index + match.length;
    }
    if (cursor < value.length) fragment.append(value.slice(cursor));
    text.replaceWith(fragment);
  }
  for (const anchor of root.querySelectorAll<HTMLAnchorElement>("a:not(.gh-ref)")) {
    if (anchor.textContent?.trim() !== anchor.getAttribute("href")?.trim()) continue;
    const ref = referenceFromUrl(anchor.getAttribute("href") ?? "");
    if (!ref) continue;
    anchor.classList.add("gh-ref");
    anchor.dataset.ghRef = referenceKey(ref);
    anchor.title = referenceKey(ref);
  }
}

/**
 * Replace each `a.gh-ref`'s text with the referenced title. Titles are cached
 * per page load; a failed fetch leaves the plain link. Safe to run after every
 * repaint: already-unfurled anchors are skipped, and a resolved title is
 * applied to every anchor currently in the document with that reference, so a
 * region re-rendered while a fetch was in flight still gets its title. A
 * reference cited several times in one region is applied once per call.
 */
export function createReferenceUnfurler(
  fetchTitle: (ref: GitHubReference) => Promise<string | null>
): (root: ParentNode) => Promise<void> {
  const titles = new Map<string, Promise<string | null>>();
  return async (root) => {
    const pending: Promise<void>[] = [];
    const scheduled = new Set<string>();
    for (const anchor of root.querySelectorAll<HTMLAnchorElement>(
      "a.gh-ref:not([data-unfurled])"
    )) {
      const key = anchor.dataset.ghRef;
      if (!key || scheduled.has(key)) continue;
      const ref = parseReferenceKey(key);
      if (!ref) continue;
      scheduled.add(key);
      let title = titles.get(key);
      if (!title) {
        title = fetchTitle(ref);
        titles.set(key, title);
      }
      const doc = anchor.ownerDocument;
      pending.push(
        title.then((resolved) => {
          if (resolved === null) return;
          for (const target of doc.querySelectorAll<HTMLAnchorElement>(
            `a.gh-ref[data-gh-ref="${CSS.escape(key)}"]:not([data-unfurled])`
          )) {
            target.textContent = resolved;
            target.dataset.unfurled = "1";
          }
        })
      );
    }
    await Promise.all(pending);
  };
}
