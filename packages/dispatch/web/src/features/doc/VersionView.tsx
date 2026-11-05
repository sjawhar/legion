import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { Doc } from "yjs";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Ask, BlockSchema, Decision } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { watchPicturePath } from "../refs/reference-target";
import { Timestamp } from "../refs/Timestamp";
import { AskBlockCard } from "./AskBlockCard";
import { changeMarksAgainst } from "./change-marks";
import { colorForLogin } from "./connection";
import { setDecisionNotes } from "./decision-notes";
import { createDoc, type EditorHandle } from "./editor";
import type { Highlight } from "./highlight";
import { embedHighlight } from "./highlight";
import { refocusAfterDiagrams } from "./refocus-after-diagrams";
import { DocumentRuntime } from "./runtime";
import type { AskBlockHost } from "./typed-block-drawing";

const NO_DECISIONS: readonly Decision[] = [];

export interface VersionViewProps {
  artifactId: string;
  /** The document's block-indexed asks, so a historical decision block still names its asker. */
  asks: readonly Ask[];
  decisions?: readonly Decision[];
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
  decisions = NO_DECISIONS,
  highlight,
  owner,
  version,
}: VersionViewProps): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const editorRef = useRef<EditorHandle | undefined>(undefined);
  const decisionsRef = useRef(decisions);
  decisionsRef.current = decisions;
  const [notesError, setNotesError] = useState<string>();
  const renderDecisionNotes = useCallback(
    (editor: EditorHandle, nodes: readonly Decision[]) => {
      setDecisionNotes(editor.view, artifactId, nodes, true);
      if (nodes.length === 0) return;
      void warmMarkdownRenderer()
        .then(() => {
          if (editorRef.current === editor && decisionsRef.current === nodes) {
            setDecisionNotes(editor.view, artifactId, nodes, true);
            setNotesError(undefined);
          }
        })
        .catch((error: unknown) => {
          if (editorRef.current === editor && decisionsRef.current === nodes)
            setNotesError(error instanceof Error ? error.message : String(error));
        });
    },
    [artifactId]
  );
  useEffect(() => {
    if (editorRef.current !== undefined) renderDecisionNotes(editorRef.current, decisions);
  }, [decisions, renderDecisionNotes]);
  const [askBlockHosts, setAskBlockHosts] = useState<readonly AskBlockHost[]>([]);
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
        onAskBlockHosts: setAskBlockHosts,
        readOnly: true,
        user: { color: colorForLogin(user.login), name: user.login },
        watchPicture: (route, onPicture) => watchPicturePath(queryClient, route, onPicture),
        ydoc: doc,
      });
      handle = editor;
      if (!mounted) {
        editor.destroy();
        return;
      }
      const embedded =
        highlighted === undefined ? undefined : embedHighlight(markdown, highlighted);
      editor.setMarkdown(embedded ?? markdown);
      editorRef.current = editor;
      renderDecisionNotes(editor, decisionsRef.current);
      setHighlightMissing(highlighted !== undefined && embedded === undefined);
      if (embedded !== undefined && highlighted !== undefined) {
        editor.focusMark(highlighted.id);
        // A diagram above the quote that draws after this focus pushes it down, so the focus
        // runs again once the diagrams have drawn; the cleanup drops it once unmounted.
        disposeRefocus = refocusAfterDiagrams(() => editor.focusMark(highlighted.id));
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
      if (mounted) {
        setRenderError(error instanceof Error ? error.message : String(error));
      }
    });

    return () => {
      mounted = false;
      editorRef.current = undefined;
      disposeRefocus();
      handle?.destroy();
      ydoc?.destroy();
    };
  }, [
    blockSchema,
    changeBaseline,
    createEditor,
    highlighted,
    markdown,
    queryClient,
    renderDecisionNotes,
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
      {notesError === undefined ? null : (
        <p role="alert">Could not load historical decision relationships: {notesError}</p>
      )}
      <article aria-label="Document" className="dispatch-doc" data-read-only="true">
        <div ref={root} />
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
