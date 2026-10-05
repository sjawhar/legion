/**
 * Resolves CommonMark reference-style links and images for the headless parser (`lib-headless`),
 * which the display surfaces read with. The live document editor resolves them through its own
 * commonmark preset's `remark-inline-links`; the headless parser is built from the schema plugins
 * alone and carries no such transform, so it needs this one.
 *
 * `[text][id]`, `[text][]` and `[text]` name a definition written elsewhere in the body,
 * `[id]: url "title"`, and `![alt][id]` does the same for an image. remark parses each into a
 * `linkReference` / `imageReference` node beside a `definition` node, which the headless schema
 * has no parser for: the parse throws, the whole body falls back to its literal source, and the
 * throw leaves the parser mid-document. People and agents write these links (a bibliography at the
 * end of a message, a generated report), so this transformer replaces each reference with the
 * inline `link` or `image` node its definition describes, as CommonMark renders it, and drops the
 * definitions, which render as nothing. micromark emits a reference node only when its definition
 * exists; a reference with no definition it leaves as the literal text its author wrote, so this
 * transformer never sees one.
 */

import { definitions } from 'mdast-util-definitions';

type MdastNode = {
  type: string;
  value?: string;
  identifier?: string;
  url?: string;
  title?: string | null;
  alt?: string | null;
  children?: MdastNode[];
};

type DefinitionNode = { url: string; title?: string | null };
type GetDefinition = (identifier?: string) => DefinitionNode | undefined;

function resolve(node: MdastNode, getDefinition: GetDefinition): void {
  if (!node.children) return;
  const resolved: MdastNode[] = [];
  for (const child of node.children) {
    if (child.type === 'definition') continue;
    if (child.type === 'linkReference' || child.type === 'imageReference') {
      const definition = getDefinition(child.identifier);
      // micromark only emits a reference node when its definition exists, and the getter reads
      // that same definition by the same normalized identifier, so this is never undefined.
      // Leaving an unresolved node in place surfaces as the body's literal-text fallback rather
      // than silently dropping the reference.
      if (definition === undefined) {
        resolved.push(child);
        continue;
      }
      if (child.type === 'imageReference') {
        resolved.push({ type: 'image', url: definition.url, title: definition.title ?? null, alt: child.alt ?? null });
      } else {
        resolve(child, getDefinition);
        resolved.push({ type: 'link', url: definition.url, title: definition.title ?? null, children: child.children ?? [] });
      }
      continue;
    }
    resolve(child, getDefinition);
    resolved.push(child);
  }
  node.children = resolved;
}

/** remark transformer, applied on markdown import. */
export function remarkResolveReferenceLinks() {
  return (tree: MdastNode) => {
    resolve(tree, definitions(tree as never) as unknown as GetDefinition);
  };
}
