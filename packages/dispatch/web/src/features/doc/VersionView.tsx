import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useRef, useState } from "react";
import type { Doc } from "yjs";

import { api } from "../../api/client";
import type { Ask, BlockSchema } from "../../api/types";
import { Timestamp } from "../refs/Timestamp";
import { AskBlockCard } from "./AskBlockCard";
import { type AskBlockHost, installAskBlockView, renderTypedBlock } from "./ask-block";
import { colorForLogin } from "./connection";
import { createDoc, type EditorHandle } from "./editor";
import type { Highlight } from "./highlight";
import { embedHighlight } from "./highlight";
import { DocumentRuntime } from "./runtime";

export interface VersionViewProps {
  artifactId: string;
  /** The document's block-indexed asks, so a historical decision block still names its asker. */
  asks: readonly Ask[];
  blockSchema: BlockSchema | undefined;
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
  createdAt,
  highlight,
  owner,
  version,
}: VersionViewProps): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const [askBlockHosts, setAskBlockHosts] = useState<readonly AskBlockHost[]>([]);
  const [highlightMissing, setHighlightMissing] = useState(false);
  const [renderError, setRenderError] = useState<string | undefined>(undefined);
  const { createEditor } = useContext(DocumentRuntime);
  const versionQuery = useQuery({
    queryKey: ["artifact", artifactId, "version", version],
    queryFn: () => api.getArtifactVersion(artifactId, version),
  });
  const userQuery = useQuery({ queryKey: ["whoami"], queryFn: () => api.whoAmI() });
  const markdown =
    versionQuery.data !== undefined && "markdown" in versionQuery.data
      ? versionQuery.data.markdown
      : undefined;

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
        renderBlock: renderTypedBlock,
        user: { color: colorForLogin(user.login), name: user.login },
        ydoc: doc,
      });
      handle = editor;
      if (!mounted) {
        editor.destroy();
        return;
      }
      installAskBlockView(editor.view, setAskBlockHosts);
      const embedded = highlight === undefined ? undefined : embedHighlight(markdown, highlight);
      editor.setMarkdown(embedded ?? markdown);
      setHighlightMissing(highlight !== undefined && embedded === undefined);
      if (embedded !== undefined && highlight !== undefined) {
        editor.focusMark(highlight.id);
      }
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
      handle?.destroy();
      ydoc?.destroy();
    };
  }, [blockSchema, createEditor, highlight, markdown, userQuery.data]);

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
            />
          );
        })}
      </article>
    </section>
  );
}
