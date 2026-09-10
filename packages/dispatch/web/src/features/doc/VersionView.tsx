import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useRef, useState } from "react";
import * as Y from "yjs";

import { api } from "../../api/client";
import { Timestamp } from "../refs/Timestamp";
import { colorForLogin } from "./connection";
import type { EditorHandle } from "./editor";
import type { Highlight } from "./highlight";
import { embedHighlight } from "./highlight";
import { DocumentRuntime } from "./runtime";

export interface VersionViewProps {
  artifactId: string;
  createdAt: string | undefined;
  highlight: Highlight | undefined;
  version: number;
}

export function VersionView({
  artifactId,
  createdAt,
  highlight,
  version,
}: VersionViewProps): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const [highlightMissing, setHighlightMissing] = useState(false);
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
    if (parent === null || markdown === undefined || user === undefined) {
      return;
    }

    const ydoc = new Y.Doc();
    let handle: EditorHandle | undefined;
    let mounted = true;
    void createEditor(parent, {
      awareness: null,
      heatMapMode: "hidden",
      readOnly: true,
      user: { color: colorForLogin(user.login), name: user.login },
      ydoc,
    }).then((editor) => {
      handle = editor;
      if (!mounted) {
        editor.destroy();
        return;
      }
      const embedded = highlight === undefined ? undefined : embedHighlight(markdown, highlight);
      editor.setMarkdown(embedded ?? markdown);
      setHighlightMissing(highlight !== undefined && embedded === undefined);
      if (embedded !== undefined && highlight !== undefined) {
        editor.focusMark(highlight.id);
      }
    });

    return () => {
      mounted = false;
      handle?.destroy();
      ydoc.destroy();
    };
  }, [createEditor, highlight, markdown, userQuery.data]);

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
            <Timestamp at={createdAt} />
          </span>
        )}
      </h2>
      {highlightMissing ? (
        <p role="status">Text changed. The selected range no longer exists in this document.</p>
      ) : null}
      <article aria-label="Document" className="dispatch-doc" data-read-only="true">
        <div ref={root} />
      </article>
    </section>
  );
}
