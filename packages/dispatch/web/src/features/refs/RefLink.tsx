import { type ReactNode, useState } from "react";

import { borderDefault } from "../../theme/classes";
import { useReferenceTarget } from "./reference-target";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  questionReference,
  referenceRouteFromHref,
  textReferences,
} from "./routes";

export interface ReferenceAnchor {
  /** The element the `RefLink` portal renders into: the `<a>` itself, or whatever a surface put
   *  in its place (`MarkdownPreview` keeps no link inside the link it sits in). */
  readonly anchor: HTMLElement;
  readonly key: string;
  readonly route: DispatchReferenceRoute;
  /** The words the anchor showed before they were cleared for the portal: a bare reference's
   *  `dispatch://…`, a bare key's `CORE-12`, a `#id`'s `#q2`, or the text a Markdown link gave
   *  it. */
  readonly text: string;
}

/** What the references a body writes bare name, once `useRenderedMarkdown` has asked: the issue
 *  keys whose issue exists (its read answered 200), and the question map of the document the
 *  body's text belongs to, each block id to its ask's `dispatch://` reference. */
export interface BareReferences {
  readonly issues: ReadonlySet<string>;
  readonly questions: ReadonlyMap<string, string>;
}

/** The 40 px square a picture shows as beside its title, wherever a line holds it: the Artifacts
 *  tab's thumbnail. `not-prose` keeps Markdown's figure margins off it. */
export const pictureThumbnailClassName = `not-prose inline-block h-10 w-10 shrink-0 rounded border object-cover align-middle ${borderDefault}`;

/** A target's title as words: an issue's key in semibold, then the rest of its title, as an issue
 *  list row draws it (`features/project/IssueList.tsx`), so its accessible name reads
 *  `CORE-12 Fix the thing`; any other title as it is. */
export function ReferenceTitleWords({
  title,
  titleKey,
}: {
  title: string;
  titleKey: string | undefined;
}): ReactNode {
  if (titleKey === undefined || !title.startsWith(titleKey)) {
    return title;
  }
  return (
    <>
      <span className="font-semibold">{titleKey}</span>
      {title.slice(titleKey.length)}
    </>
  );
}

/** Portal content for an inline reference anchor: the target's title once `useReferenceTarget`
 * has it, and until then, or for a target that does not exist, the words the anchor was written
 * with (`text`; the reference's address when the link had none). Beside it the picture's
 * thumbnail when the reference names an image artifact. A thumbnail the browser cannot draw (a
 * pinned version the server does not serve) is dropped, leaving the title alone, so a stale or
 * hand-written reference never shows a broken-image glyph. */
export function RefLink({
  route,
  text,
}: {
  route: DispatchReferenceRoute;
  text: string;
}): ReactNode {
  const { picture, title, titleKey } = useReferenceTarget(route);
  // The address that failed, not a flag: a plain reference names the latest version, so a version
  // uploaded since gets its own load instead of inheriting the last one's failure.
  const [failedPicture, setFailedPicture] = useState<string>();
  return (
    <>
      {picture === undefined || picture === failedPicture ? null : (
        <>
          <img
            alt=""
            className={pictureThumbnailClassName}
            loading="lazy"
            onError={() => setFailedPicture(picture)}
            src={picture}
          />{" "}
        </>
      )}
      {title === undefined ? (
        text === "" ? (
          buildDispatchReference(route)
        ) : (
          text
        )
      ) : (
        <ReferenceTitleWords title={title} titleKey={titleKey} />
      )}
    </>
  );
}

const excludedRefAncestorTags: Record<string, true> = { A: true, CODE: true, PRE: true };

function isInsideExcludedAncestor(node: Node, root: Node): boolean {
  for (
    let current = node.parentNode;
    current !== null && current !== root;
    current = current.parentNode
  ) {
    if (current instanceof HTMLElement && excludedRefAncestorTags[current.tagName] === true) {
      return true;
    }
  }
  return false;
}

/** The anchor a reference found in text becomes: its `href` the reference it names, its words
 *  the text written for it. */
function referenceLink(href: string, words: string): HTMLAnchorElement {
  const anchor = document.createElement("a");
  anchor.href = href;
  anchor.textContent = words;
  return anchor;
}

/**
 * Wraps every reference root's text writes bare into a real `<a>`, so `collectReferenceAnchors`
 * can then resolve it like any other link: a `dispatch://…` reference or dashboard URL that names
 * a Dispatch item; an issue key (`CORE-12`) that `bare` says names an issue that exists, as
 * `dispatch://CORE-12`; and a `#id` that names a question of the document the text belongs to
 * (`questionReference`), as its ask's reference. A reference starts and ends where the server's
 * reader says (`textReferences`); a key or `#id` `bare` does not name stays the text it was
 * written as. Text already inside a link, inline code span, or code block is left alone (its
 * ancestor already parsed as a distinct node, so there is no need to re-tokenize raw Markdown to
 * find code/link boundaries by hand). Each text node is read on its own, so a reference starts
 * and ends at a formatting boundary as it does at the start and end of the text. remark-gfm
 * already autolinks most bare `http(s)://` URLs into real link marks during Markdown parsing,
 * before this runs; a dashboard URL it leaves as text (a server at an IPv6 literal) is wrapped
 * here.
 */
export function linkifyDispatchRefs(root: HTMLElement, bare: BareReferences): void {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const candidates: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text && !isInsideExcludedAncestor(node, root)) {
      candidates.push(node);
    }
  }
  const readsBare = bare.issues.size > 0 || bare.questions.size > 0;
  for (const textNode of candidates) {
    const value = textNode.data;
    if (!readsBare && !value.includes("://")) {
      continue;
    }
    const fragment = document.createDocumentFragment();
    let lastIndex = 0;
    for (const reference of textReferences(value)) {
      if (reference.start < lastIndex) {
        continue;
      }
      let anchor: HTMLAnchorElement | undefined;
      let end = reference.end;
      if (reference.kind === "address") {
        if (referenceRouteFromHref(reference.value) !== undefined) {
          anchor = referenceLink(reference.value, reference.value);
        }
      } else if (reference.kind === "key") {
        if (bare.issues.has(reference.key)) {
          anchor = referenceLink(`dispatch://${reference.key}`, reference.key);
        }
      } else {
        const question = questionReference(reference.id, bare.questions);
        if (question !== undefined) {
          end = reference.start + 1 + question.id.length;
          anchor = referenceLink(question.ask, `#${question.id}`);
        }
      }
      if (anchor === undefined) {
        continue;
      }
      fragment.appendChild(document.createTextNode(value.slice(lastIndex, reference.start)));
      fragment.appendChild(anchor);
      lastIndex = end;
    }
    if (lastIndex === 0) {
      continue;
    }
    fragment.appendChild(document.createTextNode(value.slice(lastIndex)));
    textNode.replaceWith(fragment);
  }
}

/**
 * Finds every already-rendered `<a>` in root whose href is a dispatch:// reference or a
 * same-origin dashboard path — a link `linkifyDispatchRefs` just inserted, an ordinary Markdown
 * link a user wrote by hand, or a bare `http(s)://` URL remark-gfm autolinked — rewrites its href
 * to the SPA route via `buildReferencePath`, and clears its text so the caller can
 * portal a `RefLink` in to render the resolved title. An external link, or an href that fails to
 * parse as a reference, is left untouched, and so is a picture's link (`data-dispatch-picture`,
 * the engine's `pictureSerializer`), whose content is a `DispatchPicture` rather than a title.
 *
 * `@legion/proof-editor`'s Markdown link serializer sanitizes a `dispatch://` href to `""`
 * (Milkdown's link sanitizer only allows http/https/mailto/tel/ftp — a document strangers can
 * edit should never render an attacker-chosen non-http scheme as a clickable href), but tags the
 * anchor with `data-dispatch-href` carrying the original target. `linkifyDispatchRefs`'s own
 * freshly-wrapped anchors, and a same-origin dashboard URL a remark-gfm autolinked, still carry
 * their real value in `href` — `data-dispatch-href` takes priority when present.
 */
export function collectReferenceAnchors(
  root: HTMLElement,
  appOrigin: string = window.location.origin
): ReferenceAnchor[] {
  const targets: ReferenceAnchor[] = [];
  let index = 0;
  for (const anchor of root.querySelectorAll<HTMLAnchorElement>("a:not([data-dispatch-picture])")) {
    const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
    if (href === null) {
      continue;
    }
    const route = referenceRouteFromHref(href, appOrigin);
    if (route === undefined) {
      continue;
    }
    anchor.setAttribute("href", buildReferencePath(route));
    const reference = buildDispatchReference(route);
    anchor.setAttribute("data-dispatch-ref", reference);
    const text = anchor.textContent ?? "";
    anchor.replaceChildren();
    targets.push({ anchor, key: `${reference}:${index}`, route, text });
    index += 1;
  }
  return targets;
}
