/**
 * Linked images: an image that is also a link, such as a README badge
 * `[![build](badge.svg)](https://ci.example)`, keeps its link (INFRA-190).
 *
 * The link is stored as two attributes of the image node, `link` and `linkTitle`, rather than as
 * a link mark on the image: y-prosemirror stores an inline atom as an element holding its
 * attributes and nothing else, so a mark on an image is lost on the first save and reload, where
 * an attribute survives it.
 *
 * - `remarkLinkedImages` reads a link whose children are exactly one image, with a non-empty
 *   destination, as that image carrying the link in `data.link` / `data.linkTitle`. Every other
 *   link stays as remark reads it: a link around text and an image, a label that adds whitespace,
 *   a bold or italic image, an empty destination. It runs after reference-style links are
 *   resolved and after the dispatch-mark transform, so a linked image inside an Ask span is read
 *   the same way.
 * - `linkedImageSchema` adds the two attributes to the commonmark image, reads them from that
 *   mdast data, and writes a linked image back as the link around the image.
 * - Its DOM is the `<img>` carrying the link as `data-link` / `data-link-title`, wrapped in the
 *   link's anchor when the link is safe to navigate to (Milkdown's own `sanitizeLinkHref`) or a
 *   `dispatch://` target; an unsafe link draws no anchor at all.
 * - `linkedImageLinkSchema` makes the link mark's `a[href]` rule skip an anchor whose only content
 *   is an image, so a pasted or copied linked image carries its attributes and no link mark.
 */

import type { MilkdownPlugin } from "@milkdown/kit/ctx";
import type { Attrs, DOMOutputSpec, TagParseRule } from "@milkdown/kit/prose/model";
import type { MarkSchema, NodeSchema, Root } from "@milkdown/kit/transformer";
import { $remark } from "@milkdown/kit/utils";
import {
  imageAttr,
  imageSchema,
  linkAttr,
  linkSchema,
  sanitizeLinkHref,
} from "@milkdown/preset-commonmark";
import type { Transformer } from "unified";
import { dispatchHrefAttribute } from "../../dispatch-links";

const LINK_ATTR = "link";
const LINK_TITLE_ATTR = "linkTitle";
const LINK_DOM_ATTR = "data-link";
const LINK_TITLE_DOM_ATTR = "data-link-title";

interface MdastNode {
  type: string;
  url?: string;
  title?: string | null;
  alt?: string | null;
  children?: MdastNode[];
  data?: Record<string, unknown>;
}

interface LinkedImageData {
  link?: string;
  linkTitle?: string | null;
}

function liftLinkedImages(node: MdastNode): void {
  const children = node.children;
  if (!children) return;
  for (let i = 0; i < children.length; i++) {
    const child = children[i] as MdastNode;
    // A link around exactly one image, with a destination: the image, carrying the link.
    if (
      child.type === "link" &&
      typeof child.url === "string" &&
      child.url !== "" &&
      child.children?.length === 1 &&
      child.children[0]?.type === "image"
    ) {
      const image = (child.children as MdastNode[])[0] as MdastNode;
      image.data = { ...image.data, link: child.url, linkTitle: child.title ?? null };
      children[i] = image;
      continue;
    }
    liftLinkedImages(child);
  }
}

/** Raw remark transformer factory — use directly in a `unified()` pipeline (headless). */
export function remarkLinkedImages() {
  return (tree: MdastNode) => {
    liftLinkedImages(tree);
  };
}

/** Milkdown-wrapped remark plugin — use in `.use()` after `remarkDispatchMarksPlugin` (browser). */
export const remarkLinkedImagesPlugin = $remark(
  "remarkLinkedImages",
  () => () => remarkLinkedImages() as unknown as Transformer<Root, Root>
);

function nonEmpty(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

/** An `<a href>` whose only content is one `<img>`: a linked image, not a link mark's span.
 *  Whitespace-only text around the image does not count as content. */
function isAnchorAroundOneImage(element: Element | null): element is Element {
  if (element === null || element.nodeName.toLowerCase() !== "a" || !element.hasAttribute("href")) {
    return false;
  }
  const content = Array.from(element.childNodes).filter(
    (child) =>
      child.nodeType === 1 || (child.nodeType === 3 && (child.textContent ?? "").trim() !== "")
  );
  return content.length === 1 && (content[0] as Node).nodeName.toLowerCase() === "img";
}

function linkAttrsFromDom(img: Element): { [LINK_ATTR]: string; [LINK_TITLE_ATTR]: string } {
  const own = img.getAttribute(LINK_DOM_ATTR);
  if (own) {
    return { [LINK_ATTR]: own, [LINK_TITLE_ATTR]: img.getAttribute(LINK_TITLE_DOM_ATTR) ?? "" };
  }
  const parent = img.parentElement;
  if (isAnchorAroundOneImage(parent)) {
    return {
      [LINK_ATTR]: parent.getAttribute("href") ?? "",
      [LINK_TITLE_ATTR]: parent.getAttribute("title") ?? "",
    };
  }
  return { [LINK_ATTR]: "", [LINK_TITLE_ATTR]: "" };
}

const imageParseRule: TagParseRule = {
  tag: "img[src]",
  getAttrs: (dom) => {
    const img = dom as HTMLElement;
    const alt = img.getAttribute("alt") || "";
    return {
      src: img.getAttribute("src") || "",
      alt,
      // The editor's own DOM always carries `title`, so its empty title reads back empty; an image
      // from elsewhere with no title falls back to its alt text, as Milkdown's rule does.
      title: img.hasAttribute("title") ? (img.getAttribute("title") ?? "") : alt,
      ...linkAttrsFromDom(img),
    };
  },
};

/** The commonmark image with a `link` and a `linkTitle`. */
const linkedImageSchema = imageSchema.extendSchema((prev) => (ctx): NodeSchema => {
  const base = prev(ctx);
  return {
    ...base,
    attrs: {
      ...base.attrs,
      [LINK_ATTR]: { default: "", validate: "string" },
      [LINK_TITLE_ATTR]: { default: "", validate: "string" },
    },
    parseDOM: [imageParseRule],
    toDOM: (node) => {
      const link = nonEmpty(node.attrs[LINK_ATTR]);
      const linkTitle = nonEmpty(node.attrs[LINK_TITLE_ATTR]);
      const img: DOMOutputSpec = [
        "img",
        {
          ...ctx.get(imageAttr.key)(node),
          src: node.attrs.src,
          alt: node.attrs.alt,
          title: node.attrs.title ?? "",
          [LINK_DOM_ATTR]: link,
          [LINK_TITLE_DOM_ATTR]: linkTitle,
        },
      ];
      if (link === null) return img;
      const href = sanitizeLinkHref(link);
      const dispatch = dispatchHrefAttribute(link);
      if (href === "" && Object.keys(dispatch).length === 0) return img;
      // The attributes the link mark's DOM gives a text link to the same target.
      const linkType = node.type.schema.marks.link;
      const extra = linkType
        ? ctx.get(linkAttr.key)(linkType.create({ href: link, title: linkTitle }))
        : dispatch;
      return ["a", { ...extra, href, title: linkTitle }, img];
    },
    parseMarkdown: {
      match: base.parseMarkdown.match,
      runner: (state, node, type) => {
        const data = (node as MdastNode).data as LinkedImageData | undefined;
        state.addNode(type, {
          src: node.url as string,
          alt: node.alt as string,
          title: node.title as string,
          [LINK_ATTR]: data?.link ?? null,
          [LINK_TITLE_ATTR]: data?.linkTitle ?? null,
        });
      },
    },
    toMarkdown: {
      match: base.toMarkdown.match,
      runner: (state, node) => {
        const link = nonEmpty(node.attrs[LINK_ATTR]);
        if (link === null) {
          state.addNode("image", undefined, undefined, {
            title: node.attrs.title,
            url: node.attrs.src,
            alt: node.attrs.alt,
          });
          return;
        }
        state
          .openNode("link", undefined, {
            url: link,
            title: nonEmpty(node.attrs[LINK_TITLE_ATTR]),
          })
          .addNode("image", undefined, undefined, {
            title: nonEmpty(node.attrs.title),
            url: node.attrs.src,
            alt: node.attrs.alt,
          })
          .closeNode();
      },
    },
  };
});

function skipAnchorAroundOneImage(rule: TagParseRule): TagParseRule {
  if (rule.tag !== "a[href]") return rule;
  const { getAttrs } = rule;
  return {
    ...rule,
    getAttrs: (dom): Attrs | false | null => {
      if (isAnchorAroundOneImage(dom)) return false;
      return getAttrs ? getAttrs(dom) : (rule.attrs ?? null);
    },
  };
}

/** The commonmark link mark, leaving an anchor around one image to `linkedImageSchema`. */
const linkedImageLinkSchema = linkSchema.extendSchema((prev) => (ctx): MarkSchema => {
  const base = prev(ctx);
  return {
    ...base,
    parseDOM: base.parseDOM?.map((rule) =>
      "tag" in rule ? skipAnchorAroundOneImage(rule as TagParseRule) : rule
    ),
  };
});

/** Pass to `.use()` (browser) or the headless plugin list, after the commonmark schema. */
export const linkedImageSchemas: MilkdownPlugin[] = [
  linkedImageSchema,
  linkedImageLinkSchema,
].flat();
