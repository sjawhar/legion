import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { Doc } from "yjs";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Ask, BlockSchema } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { watchPicturePath } from "../refs/reference-target";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { AskBlockCard } from "./AskBlockCard";
import { installTypedBlocks } from "./ask-block";
import { changeMarksAgainst } from "./change-marks";
import { colorForLogin } from "./connection";
import { DocumentReferences, useReferenceClick } from "./document-references";
import { createDoc, type EditorHandle } from "./editor";
import type { Highlight } from "./highlight";
import { embedHighlight } from "./highlight";
import { refocusWhenSettled } from "./refocus-when-settled";
import { DocumentRuntime } from "./runtime";
import type { AskBlockHost } from "./typed-block-drawing";

export interface VersionViewProps {
  artifactId: string;
  /** The document's block-indexed asks, so a historical decision block still names its asker. */
  asks: readonly Ask[];
  blockSchema: BlockSchema | undefined;
  /** The markdown of the version this one is marked against - the last approved one - with which
   *  the view draws what changed since it: the words each changed block gained, and each block
   *  removed since then where it stood. Without it nothing is marked. */
  changeBaseline?: string;
  createdAt: string | undefined;
  highlight: Highlight | undefined;
  /** The project document shown, for block asks with no issue to be referenced under. */
  owner: { project: string; slug: string } | undefined;
  version: number;
}

export function VersionView({
  artifactId,
  asks,
  blockSchema,
  changeBaseline,
  createdAt,
  highlight,
  owner,
  version,
}: VersionViewProps): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const [askBlockHosts, setAskBlockHosts] = useState<readonly AskBlockHost[]>([]);
  // The handle the version's references are drawn through: the titles are a render of their
  // own, so the surface that draws them re-renders with the editor it draws into.
  const [editorHandle, setEditorHandle] = useState<EditorHandle | undefined>(undefined);
  const [highlightMissing, setHighlightMissing] = useState(false);
  const [renderError, setRenderError] = useState<string | undefined>(undefined);
  const { createEditor } = useContext(DocumentRuntime);
  const versionQuery = useQuery({
    queryKey: ["artifact", artifactId, "version", version],
    queryFn: () => api.getArtifactVersion(artifactId, version),
  });
  const userQuery = useQuery(whoAmIQuery());
  const queryClient = useQueryClient();
  const markdown =
    versionQuery.data !== undefined && "markdown" in versionQuery.data
      ? versionQuery.data.markdown
      : undefined;
  // Where a `#id` in this version's text resolves: the document it is a version of, whose
  // latest version's question blocks declare the asks it names, as the server reads them.
  const textSource = useMemo(() => textSourceOf("artifact", artifactId), [artifactId]);
  const followReference = useReferenceClick();

  // `ProofDocument` renders this view and forwards `highlight`; `ArtifactDocument` above it
  // builds that object afresh on every render. So the editor effect below keys on what the
  // highlight says rather than on the object: keyed on the object, every render of
  // `ArtifactDocument` destroys the editor, builds a new one and scrolls again, so the page
  // collapses between mounts and the last scroll can run before the last editor has laid out,
  // leaving the reader at the top. Keyed on the fields, the editor mounts once per highlighted
  // comment.
  const highlightBy = highlight?.by;
  const highlightId = highlight?.id;
  const highlightQuote = highlight?.quote;
  const highlighted = useMemo(
    () =>
      highlightBy === undefined || highlightId === undefined || highlightQuote === undefined
        ? undefined
        : { by: highlightBy, id: highlightId, quote: highlightQuote },
    [highlightBy, highlightId, highlightQuote]
  );
  useEffect(() => {
    const parent = root.current;
    const user = userQuery.data;
    if (
      parent === null ||
      markdown === undefined ||
      user === undefined ||
      blockSchema === undefined
    ) {
      return;
    }

    let ydoc: Doc | undefined;
    let handle: EditorHandle | undefined;
    let mounted = true;
    let disposeRefocus = () => {};
    setRenderError(undefined);
    const mount = async () => {
      const doc = await createDoc();
      if (!mounted) {
        doc.destroy();
        return;
      }
      ydoc = doc;
      const editor = await createEditor(parent, {
        awareness: null,
        blockSchema,
        heatMapMode: "hidden",
        readOnly: true,
        user: { color: colorForLogin(user.login), name: user.login },
        ydoc: doc,
      });
      handle = editor;
      if (!mounted) {
        editor.destroy();
        return;
      }
      installTypedBlocks(editor.view, blockSchema, setAskBlockHosts, (route, onPicture) =>
        watchPicturePath(queryClient, route, onPicture)
      );
      const embedded =
        highlighted === undefined ? undefined : embedHighlight(markdown, highlighted);
      editor.setMarkdown(embedded ?? markdown);
      setHighlightMissing(highlighted !== undefined && embedded === undefined);
      // Once the version's text is in: its references are drawn over what it reads now.
      setEditorHandle(editor);
      if (embedded !== undefined && highlighted !== undefined) {
        editor.focusMark(highlighted.id);
        // A diagram or a card above the quote that draws after this focus pushes it down, so the
        // focus runs again once the drawing has settled; the cleanup drops it once unmounted.
        disposeRefocus = refocusWhenSettled(
          () => editor.focusMark(highlighted.id),
          editor.view.dom
        );
      }
      if (changeBaseline === undefined) return;
      // The baseline is parsed by the shared headless engine, which the shell warms after the
      // first paint: awaiting that load marks a version opened straight by its own URL too,
      // rather than racing the warm-up and marking nothing.
      await warmMarkdownRenderer().catch(() => undefined);
      if (!mounted) return;
      editor.setChangeDecorations(changeMarksAgainst(changeBaseline, editor.view.state.doc));
    };
    mount().catch((error: unknown) => {
      // A version the parser refuses must say so; an empty read-only editor looks like an
      // empty document.
      handle?.destroy();
      handle = undefined;
      setEditorHandle(undefined);
      if (mounted) {
        setRenderError(error instanceof Error ? error.message : String(error));
      }
    });

    return () => {
      mounted = false;
      disposeRefocus();
      handle?.destroy();
      ydoc?.destroy();
      setEditorHandle(undefined);
    };
  }, [
    blockSchema,
    changeBaseline,
    createEditor,
    highlighted,
    markdown,
    queryClient,
    userQuery.data,
  ]);

  if (versionQuery.isPending) {
    return <p>Loading version…</p>;
  }
  if (versionQuery.isError || markdown === undefined) {
    return <p>Could not load this document version.</p>;
  }

  return (
    <section aria-label={`Document version ${version}`} className="space-y-3">
      <h2>
        Version {version}
        {createdAt === undefined ? null : (
          <span>
            {" · "}
            <Timestamp at={createdAt} />
          </span>
        )}
      </h2>
      {highlightMissing ? (
        <p role="status">Text changed. The selected range no longer exists in this document.</p>
      ) : null}
      {renderError === undefined ? null : (
        <p role="alert">This version could not be rendered: {renderError}</p>
      )}
      {/* biome-ignore lint/a11y/useKeyWithClickEvents: dispatch:// link clicks bubble here; the
      editor already handles keyboard activation of its own links. */}
      <article
        aria-label="Document"
        className="dispatch-doc"
        data-read-only="true"
        onClick={followReference}
      >
        <div ref={root} />
        <DocumentReferences editor={editorHandle} source={textSource} />
        {askBlockHosts.map((host) => {
          const blockId = String(host.node.attrs.blockId);
          return (
            <AskBlockCard
              ask={asks.find((candidate) => candidate.block_id === blockId)}
              host={host}
              key={host.key}
              owner={owner}
              readOnly
              rowResolution={false}
            />
          );
        })}
      </article>
    </section>
  );
}
