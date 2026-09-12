import type { HostBlockRenderer } from "@sjawhar/proof-editor";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Ask, AuthenticatedUser, BlockSchema, Version } from "../../api/types";
import {
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { useMargin } from "../margin/Margin";
import type { MarginOwner } from "../margin/useMarginItems";
import {
  buildIssuePath,
  buildProjectPath,
  type DispatchReferenceRoute,
  isProjectRoute,
  parseDispatchReference,
} from "../refs/routes";
import { useReferenceTarget } from "../refs/Unfurl";
import type { ConnectionState } from "./connection";
import { colorForLogin } from "./connection";
import type { EditorHandle, StoredMark } from "./editor";
import type { Highlight } from "./highlight";
import { composerKindFor, markPlacements, setActiveMarkClass } from "./marks";
import { NameVersionDialog } from "./NameVersionDialog";
import { DocumentRuntime } from "./runtime";
import { loadBlockSchema } from "./schema";
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

const renderTypedBlock: HostBlockRenderer = (node) => {
  if (node.type.name !== "ask") {
    return [
      "section",
      {
        class: `proof-typed-block proof-typed-block-${node.type.name}`,
        "data-proof-block-type": node.type.name,
      },
      [
        "header",
        { "data-proof-block-summary": "" },
        ["span", { "data-proof-block-name": "" }, node.type.name],
        [
          "dl",
          { "data-proof-block-attributes": "" },
          ...Object.entries(node.attrs).flatMap(([name, value]) => [
            ["dt", {}, name],
            [
              "dd",
              { "data-proof-block-attribute": name },
              Array.isArray(value) ? JSON.stringify(value) : String(value),
            ],
          ]),
        ],
      ],
      ["div", { "data-proof-block-content": "" }, 0],
    ];
  }
  const options: string[] = [];
  if (node.lastChild?.type.name === "bullet_list") {
    node.lastChild.forEach((item) => {
      const separator = item.textContent.indexOf(": ");
      const label = (
        separator < 0 ? item.textContent : item.textContent.slice(0, separator)
      ).trim();
      if (label !== "") options.push(label);
    });
  }
  const blockId = String(node.attrs.blockId);
  const answered = node.attrs.state === "answered";
  return [
    "section",
    {
      class: "proof-typed-block proof-typed-block-ask",
      "data-proof-block-type": "ask",
      "data-dispatch-ask-block": blockId,
    },
    [
      "header",
      { class: "mb-3 flex items-center gap-2" },
      ["strong", {}, "Decision"],
      ["span", { class: "text-sm" }, String(node.attrs.urgency)],
      answered
        ? ["span", { class: "text-sm" }, `Answered by ${String(node.attrs.answered_by)}`]
        : "",
    ],
    ["div", { "data-proof-block-content": "" }, 0],
    answered
      ? ["p", { class: "mt-3 text-sm" }, String(node.attrs.answer ?? "Answered")]
      : [
          "form",
          { class: "mt-3 grid gap-2", "data-dispatch-ask-form": blockId },
          ...options.map((label) => [
            "label",
            { class: "flex min-h-11 items-center gap-2" },
            [
              "input",
              {
                name: "selected",
                type: node.attrs.multiple === true ? "checkbox" : "radio",
                value: label,
              },
            ],
            label,
          ]),
          [
            "textarea",
            { class: "min-h-11 border p-2", name: "answer", placeholder: "Your answer" },
          ],
          ["button", { class: "min-h-11 rounded px-3", type: "submit" }, "Answer"],
        ],
  ];
};

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

/** Every already-rendered dispatch:// link mark in the live editor, deduplicated by target.
 * Read-only: unlike `RefLink`'s DOM rewriting for static Markdown bodies, this never touches the
 * editor's DOM — `@sjawhar/proof-editor` exposes no decoration/markView hook to safely replace a
 * live, editable mark's rendered text, so `ReferenceTooltip` below only sets a hover tooltip. */
function collectDispatchHrefRoutes(
  root: HTMLElement
): { reference: string; route: DispatchReferenceRoute }[] {
  const routes = new Map<string, DispatchReferenceRoute>();
  for (const anchor of root.querySelectorAll("a")) {
    const href = dispatchHrefOf(anchor);
    if (href === null) {
      continue;
    }
    const route = parseDispatchReference(href);
    if (route !== undefined) {
      routes.set(href, route);
    }
  }
  return [...routes].map(([reference, route]) => ({ reference, route }));
}

/** Sets the resolved title as a hover tooltip on every editor anchor matching reference. Renders
 * nothing itself; `useReferenceTarget` drives the effect that mutates the DOM directly, the same
 * pattern `setSearchHighlights`/`setActiveMarkClass` already use for this editor surface. */
function ReferenceTooltip({
  reference,
  route,
  rootRef,
}: {
  reference: string;
  route: DispatchReferenceRoute;
  rootRef: RefObject<HTMLDivElement | null>;
}): null {
  const { title } = useReferenceTarget(route);
  useEffect(() => {
    const root = rootRef.current;
    if (root === null) {
      return;
    }
    for (const anchor of root.querySelectorAll("a")) {
      if (dispatchHrefOf(anchor) === reference) {
        anchor.title = title ?? "";
      }
    }
  }, [reference, rootRef, title]);
  return null;
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
  const [schemaReadOnly, setSchemaReadOnly] = useState(false);
  const [dispatchLinkRoutes, setDispatchLinkRoutes] = useState<
    { reference: string; route: DispatchReferenceRoute }[]
  >([]);
  const { blockSchema: runtimeBlockSchema, connect, createEditor } = useContext(DocumentRuntime);
  const navigate = useNavigate();
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
  const versionQuery = useQuery({
    enabled: version !== undefined,
    queryKey: ["artifact", artifact.id, "version", version],
    queryFn: () => api.getArtifactVersion(artifact.id, version ?? 0),
  });
  const asksQuery = useQuery({
    enabled: version === undefined,
    queryKey: owner.kind === "issue" ? ["asks", owner.key] : ["artifact", artifact.id, "asks"],
    queryFn: () =>
      owner.kind === "issue" ? api.listIssueAsks(owner.key) : api.listArtifactAsks(artifact.id),
  });
  const answerBlockAsk = useMutation({
    mutationFn: ({ ask, selected, text }: { ask: Ask; selected: string[]; text?: string }) =>
      api.answerAsk(ask.id, { selected, ...(text === undefined ? {} : { text }) }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] });
      void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id, "text"] });
      if (owner.kind === "issue") {
        void queryClient.invalidateQueries({ queryKey: ["asks", owner.key] });
      } else {
        void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id, "asks"] });
      }
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    },
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
  const blockSchema: BlockSchema | undefined = runtimeBlockSchema ?? blockSchemaQuery.data;
  userRef.current = user;

  const requestNamedVersion = useCallback(() => {
    setIsNameDialogOpen(true);
  }, []);
  const submitBlockAnswer = (event: FormEvent<HTMLElement>) => {
    event.preventDefault();
    if (isClosed || schemaReadOnly) {
      return;
    }
    const form = event.target as HTMLFormElement;
    const blockID = form.dataset.dispatchAskForm;
    if (blockID === undefined) {
      return;
    }
    const ask = asksQuery.data?.find(
      (candidate) => candidate.block_id === blockID && candidate.block_artifact?.id === artifact.id
    );
    if (ask === undefined) {
      return;
    }
    const data = new FormData(form);
    const selected = data
      .getAll("selected")
      .filter((value): value is string => typeof value === "string");
    const text = String(data.get("answer") ?? "").trim();
    answerBlockAsk.mutate({ ask, selected, ...(text === "" ? {} : { text }) });
  };
  const openBlockAsks = (asksQuery.data ?? []).filter(
    (ask) =>
      ask.state === "open" &&
      typeof ask.block_id === "string" &&
      ask.block_artifact?.id === artifact.id
  );

  const copyBlockLink = useCallback(async () => {
    const blockId = editorRef.current?.blockIdAtSelection();
    if (blockId === null || blockId === undefined) {
      return;
    }
    await navigator.clipboard.writeText(
      `${window.location.origin}${window.location.pathname}#b-${blockId}`
    );
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
    if (blockSchema === undefined) {
      return;
    }
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
    setDispatchLinkRoutes([]);
    schemaReadOnlyRef.current = false;
    setSchemaReadOnly(false);
    const document = connect(artifact.id, {
      schemaVersion: blockSchema.version,
      onAdmission: (readOnly) => {
        schemaReadOnlyRef.current = readOnly;
        setSchemaReadOnly(readOnly);
        editor?.setReadOnly(isClosedRef.current || readOnly);
      },
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
          readOnly: isClosedRef.current || schemaReadOnlyRef.current,
          renderBlock: renderTypedBlock,
          user: {
            color: colorForLogin(userRef.current.login),
            name: userRef.current.login,
          },
          blockSchema,
          ydoc: document.doc,
        }).then((handle) => {
          if (!mounted) {
            handle.destroy();
            return;
          }
          editor = handle;
          editorRef.current = handle;
          const blockLink = window.location.hash;
          if (blockLink.startsWith("#b-")) {
            handle.focusBlock(decodeURIComponent(blockLink.slice(3)));
          }
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
          let referenceFrame = 0;
          const refreshDispatchLinks = () => {
            cancelAnimationFrame(referenceFrame);
            referenceFrame = requestAnimationFrame(() => {
              setDispatchLinkRoutes(collectDispatchHrefRoutes(handle.view.dom));
            });
          };
          refreshDispatchLinks();
          fragment.observeDeep(refreshDispatchLinks);
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
            cancelAnimationFrame(referenceFrame);
            resizeObserver.disconnect();
            marks.unobserve(project);
            fragment.unobserveDeep(publishPlacements);
            fragment.unobserveDeep(refreshSearchHighlights);
            fragment.unobserveDeep(refreshDispatchLinks);
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
  }, [artifact.id, blockSchema, connect, createEditor]);

  useEffect(() => {
    editorRef.current?.setReadOnly(isClosed || schemaReadOnlyRef.current);
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
      {blockSchemaQuery.isError || schemaReadOnly ? <p>Reload to edit.</p> : null}
      {version === undefined && openBlockAsks.length > 0 ? (
        <nav aria-label="Open decisions" className="flex flex-wrap items-center gap-2">
          <strong>{openBlockAsks.length} open decisions</strong>
          {openBlockAsks.map((ask) => (
            <a
              className={`inline-flex min-h-11 items-center rounded border px-3 ${secondaryButtonBorder} ${secondaryButtonHoverBorder} ${secondaryButtonText}`}
              href={`#b-${encodeURIComponent(ask.block_id ?? "")}`}
              key={ask.id}
              onClick={(event) => {
                event.preventDefault();
                const blockID = ask.block_id;
                if (blockID === undefined || blockID === null) {
                  return;
                }
                window.location.hash = `b-${encodeURIComponent(blockID)}`;
                editorRef.current?.focusBlock(blockID);
              }}
            >
              {ask.question}
            </a>
          ))}
        </nav>
      ) : null}
      {/* biome-ignore lint/a11y/useKeyWithClickEvents: delegates to the rendered <a> elements,
      which are already keyboard-operable — Enter on a focused link fires a click that bubbles here. */}
      <article
        aria-label="Document"
        className="dispatch-doc"
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
          navigate(isProjectRoute(route) ? buildProjectPath(route) : buildIssuePath(route));
        }}
        onSubmit={submitBlockAnswer}
      >
        <div ref={root} />
        {dispatchLinkRoutes.map(({ reference, route }) => (
          <ReferenceTooltip key={reference} reference={reference} route={route} rootRef={root} />
        ))}
      </article>
      {version === undefined ? (
        <button
          className={`min-h-11 rounded border px-3 ${secondaryButtonBorder} ${secondaryButtonDisabledText} ${secondaryButtonHoverBorder} ${secondaryButtonText}`}
          onClick={() => void copyBlockLink()}
          type="button"
        >
          Copy link to block
        </button>
      ) : null}
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
            blockSchema={blockSchema}
            createdAt={versions.find((item) => item.number === version)?.created_at}
            highlight={highlight}
            version={version}
          />
        </div>
      )}
    </section>
  );
}
