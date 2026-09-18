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
import { Link, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, AuthenticatedUser, BlockSchema, Version } from "../../api/types";
import { copyText } from "../../lib/clipboard";
import {
  badgeMed,
  dangerText,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { versionsNewestFirst } from "../artifacts/ArtifactHeader";
import { useMargin } from "../margin/Margin";
import type { MarginOwner } from "../margin/useMarginItems";
import {
  buildIssuePath,
  buildProjectPath,
  buildReferencePath,
  parseDispatchReference,
} from "../refs/routes";
import { AskBlockCard } from "./AskBlockCard";
import { type AskBlockHost, installAskBlockView, renderTypedBlock } from "./ask-block";
import type { ConnectionState, DocumentConnection } from "./connection";
import { colorForLogin } from "./connection";
import type { EditorHandle, StoredMark } from "./editor";
import type { Highlight } from "./highlight";
import {
  blockOffsets,
  blockPlacements as collectBlockPlacements,
  composerKindFor,
  markPlacements,
  setActiveBlockClass,
  setActiveMarkClass,
} from "./marks";
import { NameVersionDialog } from "./NameVersionDialog";
import { DocumentRuntime } from "./runtime";
import { loadBlockSchema } from "./schema";
import { VersionDiff } from "./VersionDiff";
import { VersionView } from "./VersionView";

const CONTROL_CHARACTERS = /\p{Cc}/gu;

/** The document chrome reports its state upward so an issue can place its controls in the active
 * Spec tab row. Project document pages retain their artifact-header toolbar. */
export interface DocumentToolbar {
  connection: ConnectionState;
  isNamingVersion: boolean;
  requestNamedVersion(): void;
  copyBlockLink(): Promise<boolean>;
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

/** A rendered link mark's `dispatch://` target, if any. `@sjawhar/proof-editor`'s Markdown
 * serializer sanitizes a `dispatch://` href to `""` (Milkdown's link sanitizer only allows
 * http/https/mailto/tel/ftp — a document strangers can edit should never render an
 * attacker-chosen non-http scheme as a clickable href) and tags the anchor with
 * `data-dispatch-href` carrying the original target instead. */
function dispatchHrefOf(anchor: Element): string | null {
  const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
  return href?.startsWith("dispatch://") ? href : null;
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
  const schemaReadOnlyRef = useRef(false);
  const userRef = useRef(user);
  const highlightTermRef = useRef(highlightTerm);
  const [connection, setConnection] = useState<ConnectionState>("connecting");
  const [loadError, setLoadError] = useState<string | undefined>(undefined);
  const [schemaReadOnly, setSchemaReadOnly] = useState(false);
  const [openDecisionIndex, setOpenDecisionIndex] = useState(0);
  const {
    blockSchema: runtimeBlockSchema,
    createEditor,
    loadTransport,
  } = useContext(DocumentRuntime);
  const navigate = useNavigate();
  const margin = useMargin();
  const { blockFocusRequest, blockPlacements } = margin;
  // The editor effect and its callbacks reach the margin's latest functions through this ref
  // rather than listing them as dependencies, so a margin re-render never rebuilds the editor.
  const marginRef = useRef(margin);
  marginRef.current = margin;
  highlightTermRef.current = highlightTerm;
  const queryClient = useQueryClient();
  const blockSchemaQuery = useQuery({
    queryFn: loadBlockSchema,
    queryKey: ["block-schema"],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const artifactQuery = useQuery({
    queryKey: ["artifact", artifact.id],
    queryFn: () => api.getArtifact(artifact.id),
  });
  const liveTextQuery = useQuery({
    queryKey: ["artifact", artifact.id, "text"],
    queryFn: () => api.getArtifactText(artifact.id),
  });
  const blockReferencesQuery = useQuery({
    enabled: version === undefined,
    queryKey: ["artifact", artifact.id, "blocks"],
    queryFn: () => api.getArtifactBlocks(artifact.id),
  });
  const versionQuery = useQuery({
    enabled: version !== undefined,
    queryKey: ["artifact", artifact.id, "version", version],
    queryFn: () => api.getArtifactVersion(artifact.id, version ?? 0),
  });
  const asksQuery = useQuery({
    queryKey: owner.kind === "issue" ? ["asks", owner.key] : ["artifact", artifact.id, "asks"],
    queryFn: () =>
      owner.kind === "issue" ? api.listIssueAsks(owner.key) : api.listArtifactAsks(artifact.id),
  });
  /** Every ask indexed from a typed block of this document, in every state — the rendered blocks
   * read who asked from these, and an answered block still names its asker. */
  const blockAsks = useMemo(
    () =>
      (asksQuery.data ?? []).filter(
        (ask) => typeof ask.block_id === "string" && ask.block_artifact?.id === artifact.id
      ),
    [asksQuery.data, artifact.id]
  );
  const blocksReadOnly = isClosed || schemaReadOnly;
  const [askBlockHosts, setAskBlockHosts] = useState<readonly AskBlockHost[]>([]);
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
    () => versionsNewestFirst(artifactQuery.data?.versions ?? artifact.versions),
    [artifactQuery.data?.versions, artifact.versions]
  );
  isClosedRef.current = isClosed;
  const blockSchema: BlockSchema | undefined = runtimeBlockSchema ?? blockSchemaQuery.data;
  userRef.current = user;

  const requestNamedVersion = useCallback(() => {
    setIsNameDialogOpen(true);
  }, []);
  // The answer itself goes through the shared ask card; the document's own reads (text, blocks,
  // versions) refresh once it lands, since the server writes the outcome into the block.
  const blockAnswered = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] });
  }, [queryClient, artifact.id]);
  const openBlockAsks = useMemo(
    () =>
      blockAsks
        .filter((ask) => ask.state === "open")
        .map((ask) => ({
          ...ask,
          question: ask.question.replace(CONTROL_CHARACTERS, ""),
        })),
    [blockAsks]
  );

  const copyBlockLink = useCallback(async (): Promise<boolean> => {
    const blockId = editorRef.current?.blockIdAtSelection();
    if (blockId === null || blockId === undefined) {
      return false;
    }
    return copyText(`${window.location.origin}${window.location.pathname}#b-${blockId}`);
  }, []);
  useEffect(() => {
    setOpenDecisionIndex((current) => Math.min(current, Math.max(openBlockAsks.length - 1, 0)));
  }, [openBlockAsks.length]);

  const openDecision = openBlockAsks[Math.min(openDecisionIndex, openBlockAsks.length - 1)];

  useEffect(() => {
    onToolbarChange?.({
      connection,
      copyBlockLink,
      isNamingVersion: nameVersion.isPending,
      requestNamedVersion,
      versions,
    });
  }, [
    connection,
    copyBlockLink,
    nameVersion.isPending,
    onToolbarChange,
    requestNamedVersion,
    versions,
  ]);

  // Separate from the reporting effect above (whose cleanup would otherwise fire — and
  // transiently clear the parent's toolbar — on every dependency change, not just on unmount).
  // This one's only job is telling the parent there is no longer a toolbar to show once this
  // artifact's instance is gone, so switching documents can't leave the header holding a stale
  // `requestNamedVersion` that would name a version on the artifact the user navigated away from.
  useEffect(() => {
    return () => onToolbarChange?.(undefined);
  }, [onToolbarChange]);

  useEffect(() => {
    if (blockSchema === undefined) {
      return;
    }
    const parent = root.current;
    if (parent === null) {
      return;
    }

    let mounted = true;
    let document: DocumentConnection | undefined;
    let editor: EditorHandle | undefined;
    let disposeEditorBindings: (() => void) | undefined;
    setConnection("connecting");
    setLoadError(undefined);
    schemaReadOnlyRef.current = false;
    setSchemaReadOnly(false);
    // A transport or editor chunk that fails while online (after `DeploymentResilience` has
    // spent its reload) would otherwise leave the document "connecting" forever. Once failed,
    // the live provider's later status events must not repaint the dot over the alert.
    let failed = false;
    const reportLoadFailure = (error: unknown) => {
      if (!mounted) {
        return;
      }
      failed = true;
      setConnection("failed");
      setLoadError(error instanceof Error ? error.message : String(error));
    };
    void loadTransport()
      .then((connect) => {
        if (!mounted) {
          return;
        }
        const connection = connect(artifact.id, {
          schemaVersion: blockSchema.version,
          onAdmission: (readOnly) => {
            schemaReadOnlyRef.current = readOnly;
            setSchemaReadOnly(readOnly);
            editor?.setReadOnly(isClosedRef.current || readOnly);
          },
          onStatus: (status) => {
            if (!failed) {
              setConnection(status);
            }
          },
          onSynced: () => {
            void createEditor(parent, {
              // Cursor labels are outside the compact acceptance bar. Even with inline labels,
              // yCursor's edge widget disrupts the mobile browser's post-update text selection.
              awareness:
                globalThis.document.documentElement.clientWidth > 0 &&
                globalThis.document.documentElement.clientWidth < 1280
                  ? null
                  : connection.awareness,
              heatMapMode: "hidden",
              onMarkAction: (action) => {
                switch (action.kind) {
                  case "comment":
                  case "suggest":
                  case "ask":
                    return marginRef.current
                      .composeForMark({
                        anchor: {
                          artifact: artifact.id,
                          mark_id: action.markId,
                          quote: action.quote,
                        },
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
              onMarkClick: (markId) => marginRef.current.focusItemForMark(markId),
              onMarkHover: (markId) => marginRef.current.hoverItemForMark(markId),
              readOnly: isClosedRef.current || schemaReadOnlyRef.current,
              renderBlock: renderTypedBlock,
              user: {
                color: colorForLogin(userRef.current.login),
                name: userRef.current.login,
              },
              blockSchema,
              ydoc: connection.doc,
            })
              .then((handle) => {
                if (!mounted) {
                  handle.destroy();
                  return;
                }
                editor = handle;
                editorRef.current = handle;
                installAskBlockView(handle.view, setAskBlockHosts);
                const blockLink = window.location.hash;
                if (blockLink.startsWith("#b-")) {
                  handle.focusBlock(decodeURIComponent(blockLink.slice(3)));
                }
                setSearchHighlights(handle.view.dom, highlightTermRef.current);
                marginRef.current.registerDocument({
                  focusBlock: (blockId) => {
                    requestAnimationFrame(() => handle.focusBlock(blockId));
                  },
                  focusMark: (markId) => handle.focusMark(markId),
                  setActiveBlocks: (blockIds) => setActiveBlockClass(handle.view.dom, blockIds),
                  setActiveMarks: (markIds) => setActiveMarkClass(handle.view.dom, markIds),
                });
                const marks = connection.doc.getMap("marks");
                const project = () => {
                  handle.applyRemoteMarks(marks.toJSON() as Record<string, StoredMark>, {
                    hydrateAnchors: false,
                  });
                };
                project();
                marks.observe(project);
                const fragment = connection.doc.getXmlFragment("prosemirror");
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
                    marginRef.current.setMarkPlacements(
                      markPlacements(handle.view.state.doc, handle.markOffsets())
                    );
                    marginRef.current.setBlockPlacements(
                      collectBlockPlacements(handle.view.state.doc, blockOffsets(handle.view.dom))
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
                  marginRef.current.registerDocument(undefined);
                };
              })
              .catch(reportLoadFailure);
          },
        });
        document = connection;
      })
      .catch(reportLoadFailure);

    return () => {
      mounted = false;
      disposeEditorBindings?.();
      editor?.destroy();
      document?.destroy();
      if (editorRef.current === editor) {
        editorRef.current = undefined;
      }
    };
  }, [artifact.id, blockSchema, createEditor, loadTransport]);

  useEffect(() => {
    editorRef.current?.setReadOnly(isClosed || schemaReadOnlyRef.current);
  }, [isClosed]);

  useEffect(() => {
    if (blockFocusRequest === undefined) {
      return;
    }
    const frame = requestAnimationFrame(() => {
      editorRef.current?.focusBlock(blockFocusRequest.blockId);
    });
    return () => cancelAnimationFrame(frame);
  }, [blockFocusRequest]);

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
      {blockSchemaQuery.isError || schemaReadOnly ? <p>Reload to edit.</p> : null}
      {version === undefined && openDecision !== undefined ? (
        <nav
          aria-label="Open decisions"
          className="flex min-h-11 items-center gap-2 md:h-7 md:min-h-0"
        >
          <span
            className={`inline-flex h-6 shrink-0 items-center rounded-full px-2 text-xs font-semibold ${badgeMed.bg} ${badgeMed.text}`}
          >
            {openBlockAsks.length === 1
              ? "1 open decision"
              : `${openBlockAsks.length} open decisions`}
          </span>
          <a
            className={`block min-w-0 max-w-[90ch] truncate text-sm font-medium underline ${linkText} ${linkHoverText}`}
            href={`#b-${encodeURIComponent(openDecision.block_id ?? "")}`}
            onClick={(event) => {
              event.preventDefault();
              const blockID = openDecision.block_id;
              if (blockID === undefined || blockID === null) {
                return;
              }
              window.location.hash = `b-${encodeURIComponent(blockID)}`;
              editorRef.current?.focusBlock(blockID);
            }}
            title={openDecision.question}
          >
            {openDecision.question}
          </a>
          {openBlockAsks.length > 1 ? (
            <button
              aria-label="Next open decision"
              className={`ml-auto flex min-h-11 min-w-11 items-center justify-center rounded-full md:min-h-7 md:min-w-7 ${secondaryButtonBorder} ${secondaryButtonHoverBorder} ${secondaryButtonText}`}
              onClick={() =>
                setOpenDecisionIndex((current) => (current + 1) % openBlockAsks.length)
              }
              type="button"
            >
              <svg aria-hidden="true" className="h-3 w-3" fill="none" viewBox="0 0 16 16">
                <path d="m6 3 5 5-5 5" stroke="currentColor" strokeWidth="1.5" />
              </svg>
            </button>
          ) : null}
        </nav>
      ) : null}
      {loadError === undefined ? null : (
        <p className={dangerText} role="alert">
          This document could not load: {loadError}
        </p>
      )}
      {/* biome-ignore lint/a11y/useKeyWithClickEvents: dispatch:// link clicks bubble here; the
      editor already handles keyboard activation of its own links. */}
      <article
        aria-label="Document"
        className="dispatch-doc relative"
        data-read-only={isClosed || schemaReadOnly}
        hidden={version !== undefined}
        onClick={(event) => {
          // A modifier click (open in new tab/window) or a drag-selection that happens to end
          // on the link should reach the browser/editor's own handling, not steal the click.
          const modified = event.ctrlKey || event.metaKey || event.shiftKey || event.altKey;
          if (event.button !== 0 || modified) {
            return;
          }
          const selection = window.getSelection();
          if (selection !== null && !selection.isCollapsed) {
            return;
          }
          const anchor = (event.target as HTMLElement).closest<HTMLAnchorElement>("a");
          const href = anchor === null ? null : dispatchHrefOf(anchor);
          const route = href === null ? undefined : parseDispatchReference(href);
          if (route === undefined) {
            return;
          }
          event.preventDefault();
          navigate(buildReferencePath(route));
        }}
      >
        <div ref={root} />
        {askBlockHosts.map((host) => {
          const blockId = String(host.node.attrs.blockId);
          return (
            <AskBlockCard
              ask={blockAsks.find((candidate) => candidate.block_id === blockId)}
              host={host}
              key={host.key}
              onAnswered={blockAnswered}
              owner={owner.kind === "document" ? owner : undefined}
              readOnly={blocksReadOnly}
            />
          );
        })}
        {blockReferencesQuery.data?.some(
          (block) => block.references.comments + block.references.asks > 0
        ) ? (
          <aside
            aria-label="Block references"
            className="pointer-events-none absolute top-0 right-0 z-10 w-8"
          >
            {blockReferencesQuery.data
              .filter((block) => block.references.comments + block.references.asks > 0)
              .map((block) => {
                const count = block.references.comments + block.references.asks;
                return (
                  <button
                    aria-label={`${count} references on block`}
                    className={`pointer-events-auto absolute right-0 min-h-11 min-w-11 rounded-full text-xs font-semibold ${badgeMed.bg} ${badgeMed.text}`}
                    key={block.id}
                    onClick={() => {
                      marginRef.current.filterToBlock(block.id);
                      editorRef.current?.focusBlock(block.id);
                    }}
                    style={{ top: `${blockPlacements.get(block.id)?.top ?? 0}px` }}
                    type="button"
                  >
                    {count}
                  </button>
                );
              })}
          </aside>
        ) : null}
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
            asks={blockAsks}
            blockSchema={blockSchema}
            createdAt={versions.find((item) => item.number === version)?.created_at}
            highlight={highlight}
            owner={owner.kind === "document" ? owner : undefined}
            version={version}
          />
        </div>
      )}
    </section>
  );
}
