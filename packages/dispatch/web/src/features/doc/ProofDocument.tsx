import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, AuthenticatedUser, Version } from "../../api/types";
import { useMargin } from "../margin/Margin";
import type { MarginOwner } from "../margin/useMarginItems";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import type { ConnectionState } from "./connection";
import { colorForLogin } from "./connection";
import type { EditorHandle, StoredMark } from "./editor";
import type { Highlight } from "./highlight";
import { composerKindFor, markPlacements, setActiveMarkClass } from "./marks";
import { NameVersionDialog } from "./NameVersionDialog";
import { DocumentRuntime } from "./runtime";
import { VersionDiff } from "./VersionDiff";
import { VersionView } from "./VersionView";

/** The document chrome (version picker, Name version, Diff toggle, connection dot) used to
 * render inside this component; it now renders in the page's own header, one bar with the
 * title. `ProofDocument` reports the state that chrome needs through this bag instead of
 * rendering it itself, so the header can sit above the tabs while the document stays below. */
export interface DocumentToolbar {
  connection: ConnectionState;
  isNamingVersion: boolean;
  requestNamedVersion(): void;
  versions: Version[];
}

export interface ProofDocumentProps {
  artifact: Artifact;
  highlight: Highlight | undefined;
  highlightTerm?: string;
  isClosed: boolean;
  owner: MarginOwner;
  showDiff: boolean;
  onToolbarChange?(toolbar: DocumentToolbar | undefined): void;
  onVersionChange(version: number | null): void;
  user: AuthenticatedUser;
  version?: number;
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

interface SearchHighlightSupport {
  readonly CSS?: {
    readonly highlights?: {
      set(name: string, highlight: unknown): void;
    };
  };
  readonly Highlight?: new (...ranges: Range[]) => unknown;
}

function setSearchHighlights(root: HTMLElement, query: string): void {
  const { CSS, Highlight } = window as unknown as SearchHighlightSupport;
  if (CSS?.highlights === undefined || Highlight === undefined) {
    return;
  }
  const ranges: Range[] = [];
  if (query !== "") {
    const lowerQuery = query.toLowerCase();
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const text = node.nodeValue ?? "";
      const lowerText = text.toLowerCase();
      for (let start = lowerText.indexOf(lowerQuery); start !== -1; ) {
        const range = document.createRange();
        range.setStart(node, start);
        range.setEnd(node, start + query.length);
        ranges.push(range);
        start = lowerText.indexOf(lowerQuery, start + query.length);
      }
    }
  }
  CSS.highlights.set("dispatch-search", new Highlight(...ranges));
}

export function ProofDocument({
  artifact,
  highlight,
  highlightTerm = "",
  isClosed,
  owner,
  onToolbarChange,
  onVersionChange,
  showDiff,
  user,
  version,
}: ProofDocumentProps): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const editorRef = useRef<EditorHandle | undefined>(undefined);
  const isClosedRef = useRef(isClosed);
  const userRef = useRef(user);
  const highlightTermRef = useRef(highlightTerm);
  const [connection, setConnection] = useState<ConnectionState>("connecting");
  const { connect, createEditor } = useContext(DocumentRuntime);
  const {
    composeForMark,
    focusItemForMark,
    hoverItemForMark,
    registerDocument,
    setMarkPlacements,
  } = useMargin();
  const composeForMarkRef = useRef(composeForMark);
  const focusItemForMarkRef = useRef(focusItemForMark);
  const hoverItemForMarkRef = useRef(hoverItemForMark);
  const registerDocumentRef = useRef(registerDocument);
  const setMarkPlacementsRef = useRef(setMarkPlacements);
  composeForMarkRef.current = composeForMark;
  focusItemForMarkRef.current = focusItemForMark;
  hoverItemForMarkRef.current = hoverItemForMark;
  registerDocumentRef.current = registerDocument;
  setMarkPlacementsRef.current = setMarkPlacements;
  highlightTermRef.current = highlightTerm;
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
  const [isNameDialogOpen, setIsNameDialogOpen] = useState(false);
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
      setIsNameDialogOpen(false);
    },
  });
  const liveMarkdown = liveTextQuery.data?.markdown ?? "";
  const versionMarkdown =
    versionQuery.data !== undefined && "markdown" in versionQuery.data
      ? versionQuery.data.markdown
      : undefined;
  const versions = useMemo(
    () =>
      [...(artifactQuery.data?.versions ?? artifact.versions)].sort(
        (left, right) => right.number - left.number
      ),
    [artifactQuery.data?.versions, artifact.versions]
  );
  isClosedRef.current = isClosed;
  userRef.current = user;

  const requestNamedVersion = useCallback(() => {
    setIsNameDialogOpen(true);
  }, []);

  useEffect(() => {
    onToolbarChange?.({
      connection,
      isNamingVersion: nameVersion.isPending,
      requestNamedVersion,
      versions,
    });
  }, [connection, nameVersion.isPending, onToolbarChange, requestNamedVersion, versions]);

  // Separate from the reporting effect above (whose cleanup would otherwise fire — and
  // transiently clear the parent's toolbar — on every dependency change, not just on unmount).
  // This one's only job is telling the parent there is no longer a toolbar to show once this
  // artifact's instance is gone, so switching documents can't leave the header holding a stale
  // `requestNamedVersion` that would name a version on the artifact the user navigated away from.
  useEffect(() => {
    return () => onToolbarChange?.(undefined);
  }, [onToolbarChange]);

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
          setSearchHighlights(handle.view.dom, highlightTermRef.current);
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
          let searchFrame = 0;
          const refreshSearchHighlights = () => {
            cancelAnimationFrame(searchFrame);
            searchFrame = requestAnimationFrame(() => {
              setSearchHighlights(handle.view.dom, highlightTermRef.current);
            });
          };
          refreshSearchHighlights();
          fragment.observeDeep(refreshSearchHighlights);
          let frame = 0;
          const publishPlacements = () => {
            cancelAnimationFrame(frame);
            frame = requestAnimationFrame(() => {
              setMarkPlacementsRef.current(
                markPlacements(handle.view.state.doc, handle.markOffsets())
              );
            });
          };
          const resizeObserver = new ResizeObserver(publishPlacements);
          resizeObserver.observe(handle.view.dom);
          publishPlacements();
          fragment.observeDeep(publishPlacements);
          disposeEditorBindings = () => {
            cancelAnimationFrame(frame);
            cancelAnimationFrame(searchFrame);
            resizeObserver.disconnect();
            marks.unobserve(project);
            fragment.unobserveDeep(publishPlacements);
            fragment.unobserveDeep(refreshSearchHighlights);
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

  useEffect(() => {
    const editor = editorRef.current;
    if (editor !== undefined) {
      setSearchHighlights(editor.view.dom, highlightTerm);
    }
  }, [highlightTerm]);

  return (
    <section aria-label="Document editor" className="space-y-4">
      <NameVersionDialog
        error={nameVersion.isError}
        onClose={() => {
          nameVersion.reset();
          setIsNameDialogOpen(false);
        }}
        onSave={(summary) => nameVersion.mutate(summary)}
        open={isNameDialogOpen}
        saving={nameVersion.isPending}
      />
      {isClosed ? <p>This issue is closed. Its document is read-only.</p> : null}
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
              owner.kind === "document"
                ? buildProjectPath({
                    kind: "document",
                    project: owner.project,
                    slug: owner.slug,
                  })
                : artifact.primary
                  ? buildIssuePath({ key: owner.key, kind: "spec" })
                  : buildIssuePath({
                      key: owner.key,
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
