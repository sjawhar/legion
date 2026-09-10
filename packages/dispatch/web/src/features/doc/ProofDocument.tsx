import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, AuthenticatedUser, Version } from "../../api/types";
import { useMargin } from "../margin/Margin";
import { buildIssuePath } from "../refs/routes";
import { type ConnectionState, colorForLogin } from "./connection";
import type { EditorHandle, StoredMark } from "./editor";
import type { Highlight } from "./highlight";
import { composerKindFor, markPositions, setActiveMarkClass } from "./marks";
import { DocumentRuntime } from "./runtime";
import { VersionDiff } from "./VersionDiff";
import { VersionView } from "./VersionView";

export interface ProofDocumentProps {
  artifact: Artifact;
  highlight: Highlight | undefined;
  isClosed: boolean;
  issueKey: string;
  onVersionChange(version: number | null): void;
  user: AuthenticatedUser;
  version?: number;
}

function connectionLabel(connection: ConnectionState): string {
  return connection === "connecting" ? "Connecting to the document…" : connection;
}

function removeMarkFromDocument(editor: EditorHandle, markId: string): void {
  let transaction = editor.view.state.tr;
  editor.view.state.doc.descendants((node, position) => {
    if (!node.isText) {
      return true;
    }
    const mark = node.marks.find((candidate) => candidate.attrs.id === markId);
    if (mark !== undefined) {
      transaction = transaction.removeMark(position, position + node.nodeSize, mark);
    }
    return true;
  });
  if (transaction.docChanged) {
    editor.view.dispatch(transaction);
  }
}

export function ProofDocument({
  artifact,
  highlight,
  isClosed,
  issueKey: _issueKey,
  onVersionChange,
  user,
  version,
}: ProofDocumentProps): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const editorRef = useRef<EditorHandle | undefined>(undefined);
  const isClosedRef = useRef(isClosed);
  const userRef = useRef(user);
  const [connection, setConnection] = useState<ConnectionState>("connecting");
  const [showDiff, setShowDiff] = useState(false);
  const { connect, createEditor } = useContext(DocumentRuntime);
  const { composeForMark, focusItemForMark, hoverItemForMark, registerDocument, setMarkPositions } =
    useMargin();
  const composeForMarkRef = useRef(composeForMark);
  const focusItemForMarkRef = useRef(focusItemForMark);
  const hoverItemForMarkRef = useRef(hoverItemForMark);
  const registerDocumentRef = useRef(registerDocument);
  const setMarkPositionsRef = useRef(setMarkPositions);
  composeForMarkRef.current = composeForMark;
  focusItemForMarkRef.current = focusItemForMark;
  hoverItemForMarkRef.current = hoverItemForMark;
  registerDocumentRef.current = registerDocument;
  setMarkPositionsRef.current = setMarkPositions;
  const queryClient = useQueryClient();
  const artifactQuery = useQuery({
    queryKey: ["artifact", artifact.id],
    queryFn: () => api.getArtifact(artifact.id),
  });
  const liveTextQuery = useQuery({
    queryKey: ["artifact", artifact.id, "text"],
    queryFn: () => api.getArtifactText(artifact.id),
  });
  const versionQuery = useQuery({
    enabled: version !== undefined,
    queryKey: ["artifact", artifact.id, "version", version],
    queryFn: () => api.getArtifactVersion(artifact.id, version ?? 0),
  });
  const nameVersion = useMutation({
    mutationFn: (summary: string) => api.createArtifactVersion(artifact.id, { summary }),
    onSuccess: (created) => {
      queryClient.setQueryData<Artifact>(["artifact", artifact.id], (current) => {
        const source = current ?? artifact;
        return {
          ...source,
          versions: [...source.versions.filter(({ number }) => number !== created.number), created],
        };
      });
      onVersionChange(created.number);
      setShowDiff(false);
    },
  });
  const liveMarkdown = liveTextQuery.data?.markdown ?? "";
  const versionMarkdown =
    versionQuery.data !== undefined && "markdown" in versionQuery.data
      ? versionQuery.data.markdown
      : undefined;
  const versions = [...(artifactQuery.data?.versions ?? artifact.versions)].sort(
    (left, right) => right.number - left.number
  );
  isClosedRef.current = isClosed;
  userRef.current = user;

  useEffect(() => {
    const parent = root.current;
    if (parent === null) {
      return;
    }

    let mounted = true;
    let synced = false;
    let editor: EditorHandle | undefined;
    let disposeEditorBindings: (() => void) | undefined;
    const inspectionWindow = window as Window & {
      __dispatchDocument?: { editor: EditorHandle; view: EditorHandle["view"] };
    };
    setConnection("connecting");
    const document = connect(artifact.id, {
      onStatus: setConnection,
      onSynced: () => {
        if (synced) {
          return;
        }
        synced = true;
        void createEditor(parent, {
          // Cursor labels are outside the compact acceptance bar. Even with inline labels,
          // yCursor's edge widget disrupts the mobile browser's post-update text selection.
          awareness:
            globalThis.document.documentElement.clientWidth > 0 &&
            globalThis.document.documentElement.clientWidth < 1280
              ? null
              : document.awareness,
          heatMapMode: "hidden",
          onMarkAction: (action) => {
            switch (action.kind) {
              case "comment":
              case "suggest":
              case "ask":
                return composeForMarkRef
                  .current({
                    anchor: { artifact: artifact.id, mark_id: action.markId, quote: action.quote },
                    kind: composerKindFor(action.kind),
                  })
                  .catch((error: unknown) => {
                    if (editor === undefined) {
                      throw new Error(
                        "The editor must exist before its selection action can be cancelled."
                      );
                    }
                    removeMarkFromDocument(editor, action.markId);
                    throw error;
                  });
              default:
                throw new Error(
                  `Dispatch renders mark threads in the margin; popover action ${action.kind} cannot fire`
                );
            }
          },
          onMarkClick: (markId) => focusItemForMarkRef.current(markId),
          onMarkHover: (markId) => hoverItemForMarkRef.current(markId),
          readOnly: isClosedRef.current,
          user: {
            color: colorForLogin(userRef.current.login),
            name: userRef.current.login,
          },
          ydoc: document.doc,
        }).then((handle) => {
          if (!mounted) {
            handle.destroy();
            return;
          }
          editor = handle;
          editorRef.current = handle;
          if (import.meta.env.VITE_DISPATCH_E2E === "1") {
            inspectionWindow.__dispatchDocument = { editor: handle, view: handle.view };
          }
          registerDocumentRef.current({
            focusMark: (markId) => handle.focusMark(markId),
            setActiveMarks: (markIds) => setActiveMarkClass(handle.view.dom, markIds),
          });
          const marks = document.doc.getMap("marks");
          const project = () => {
            handle.applyRemoteMarks(marks.toJSON() as Record<string, StoredMark>, {
              hydrateAnchors: false,
            });
          };
          project();
          marks.observe(project);
          const fragment = document.doc.getXmlFragment("prosemirror");
          let frame = 0;
          const publishPositions = () => {
            cancelAnimationFrame(frame);
            frame = requestAnimationFrame(() => {
              setMarkPositionsRef.current(markPositions(handle.view.state.doc));
            });
          };
          publishPositions();
          fragment.observeDeep(publishPositions);
          disposeEditorBindings = () => {
            cancelAnimationFrame(frame);
            marks.unobserve(project);
            fragment.unobserveDeep(publishPositions);
            registerDocumentRef.current(undefined);
          };
        });
      },
    });

    return () => {
      mounted = false;
      disposeEditorBindings?.();
      editor?.destroy();
      document.destroy();
      if (editorRef.current === editor) {
        editorRef.current = undefined;
      }
      if (inspectionWindow.__dispatchDocument?.editor === editor) {
        delete inspectionWindow.__dispatchDocument;
      }
    };
  }, [artifact.id, connect, createEditor]);

  useEffect(() => {
    editorRef.current?.setReadOnly(isClosed);
  }, [isClosed]);

  const requestNamedVersion = () => {
    const summary = window.prompt("What changed in this version?");
    if (summary?.trim()) {
      nameVersion.mutate(summary.trim());
    }
  };

  return (
    <section aria-label="Document editor" className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border p-3">
        <div className="flex flex-wrap items-center gap-2">
          <button
            disabled={isClosed || nameVersion.isPending}
            onClick={requestNamedVersion}
            type="button"
          >
            Name version
          </button>
          {version === undefined ? null : (
            <button onClick={() => setShowDiff((visible) => !visible)} type="button">
              {showDiff ? "Show version" : "Diff vs current"}
            </button>
          )}
        </div>
        <div className="flex min-w-0 max-w-full flex-wrap items-center gap-3">
          <span role="status">{connectionLabel(connection)}</span>
          <label>
            Version
            <select
              aria-label="Version"
              onChange={(event) => {
                onVersionChange(event.target.value === "" ? null : Number(event.target.value));
                setShowDiff(false);
              }}
              value={version ?? ""}
            >
              <option value="">Current</option>
              {versions.map((item: Version) => (
                <option key={item.number} value={item.number}>
                  Version {item.number}
                  {item.named && item.summary !== null ? ` — ${item.summary}` : ""}
                </option>
              ))}
            </select>
          </label>
        </div>
      </div>
      {isClosed ? <p>This issue is closed. Its document is read-only.</p> : null}
      {nameVersion.isError ? <p role="alert">Could not name this version.</p> : null}
      <article
        aria-label="Document"
        className="dispatch-doc"
        data-read-only={isClosed}
        hidden={version !== undefined}
      >
        <div ref={root} />
      </article>
      {version === undefined ? null : versionQuery.isError ? (
        <section aria-label={`Document version ${version}`} className="space-y-3">
          <p>
            No version {version} of {artifact.name}.
          </p>
          <Link
            to={
              artifact.primary
                ? buildIssuePath({ key: artifact.issue_key, kind: "spec" })
                : buildIssuePath({
                    key: artifact.issue_key,
                    kind: "artifact",
                    slug: artifact.slug,
                  })
            }
          >
            View current version
          </Link>
        </section>
      ) : versionMarkdown === undefined ? (
        <p>Loading version…</p>
      ) : showDiff ? (
        <VersionDiff after={liveMarkdown} before={versionMarkdown} />
      ) : (
        <div data-testid="version-view">
          <VersionView
            artifactId={artifact.id}
            createdAt={versions.find((item) => item.number === version)?.created_at}
            highlight={highlight}
            version={version}
          />
        </div>
      )}
    </section>
  );
}
