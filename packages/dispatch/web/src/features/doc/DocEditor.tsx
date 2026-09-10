import { markdown } from "@codemirror/lang-markdown";
import { Compartment, EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { HocuspocusProvider } from "@hocuspocus/provider";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { yCollab } from "y-codemirror.next";
import * as Y from "yjs";

import { api } from "../../api/client";
import type { Artifact, AuthenticatedUser, Comment, Version } from "../../api/types";
import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  card,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  statusConnected,
  statusConnecting,
  statusOffline,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useMargin } from "../margin/Margin";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { anchorDecorationExtension, setActiveAnchorIds, setAnchorDecorations } from "./anchors";
import { DocView, type DocViewHighlight, quoteOccurrence } from "./DocView";
import { VersionDiff } from "./VersionDiff";

interface DocEditorProps {
  artifact: Artifact;
  highlight?: DocViewHighlight;
  isClosed: boolean;
  issueKey?: string;
  onVersionChange?: (version: number | null) => void;
  selectedVersion?: number | null;
  user: AuthenticatedUser;
}

type EditorMode = "edit" | "preview";
type ConnectionState = "connecting" | "connected" | "offline";

const presenceColors = ["#0284c7", "#7c3aed", "#c2410c", "#047857", "#be123c", "#4338ca"];

/** Gives each collaborator a stable, distinct cursor color in the current browser session. */
function colorForLogin(login: string): string {
  let hash = 0;
  for (const character of login) {
    hash = (hash * 31 + character.charCodeAt(0)) | 0;
  }
  return presenceColors[Math.abs(hash) % presenceColors.length] ?? presenceColors[0];
}

/** Builds the same-origin Hocuspocus endpoint without hard-coding deployment hosts. */
export function wsUrl(artifactId: string): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}/ws/doc/${encodeURIComponent(artifactId)}`;
}

function editorAccess(isClosed: boolean) {
  return [EditorState.readOnly.of(isClosed), EditorView.editable.of(!isClosed)];
}

function historicalHighlight(
  markdown: string,
  highlight: DocViewHighlight | undefined,
  anchor: Comment["anchor"] | undefined
): DocViewHighlight | undefined {
  if (highlight === undefined || anchor === null || anchor === undefined) {
    return highlight;
  }
  if (markdown.slice(highlight.from, highlight.to) === anchor.quote) {
    return highlight;
  }
  const from = markdown.indexOf(anchor.quote);
  return from === -1 || markdown.indexOf(anchor.quote, from + 1) !== -1
    ? highlight
    : { from, to: from + anchor.quote.length };
}

export function DocEditor(props: DocEditorProps): ReactNode {
  return <DocEditorContent key={props.artifact.id} {...props} />;
}

function DocEditorContent({
  artifact,
  highlight,
  isClosed,
  issueKey,
  onVersionChange,
  selectedVersion: controlledSelectedVersion,
  user,
}: DocEditorProps): ReactNode {
  const host = useRef<HTMLDivElement>(null);
  const isClosedRef = useRef(isClosed);
  const providerRef = useRef<HocuspocusProvider | null>(null);
  const userLoginRef = useRef(user.login);
  const viewRef = useRef<EditorView | null>(null);
  const readOnlyRef = useRef<Compartment | null>(null);
  const themeRef = useRef<Compartment | null>(null);
  const decoratedAnchorKey = useRef<string | undefined>(undefined);
  const awaitingAnchorDocument = useRef(false);
  isClosedRef.current = isClosed;
  userLoginRef.current = user.login;
  const { anchors, hoveredItemId, selectItem, selectedItemId, setHoveredItemId, setSelection } =
    useMargin();
  const anchorHover = useRef<(id: string | undefined) => void>(() => {});
  const anchorSelect = useRef<(id: string) => void>(() => {});
  const selectionHandler = useRef<(view: EditorView) => void>(() => {});
  anchorHover.current = setHoveredItemId;
  anchorSelect.current = selectItem;
  selectionHandler.current = (view) => {
    const { from, to } = view.state.selection.main;
    if (from === to) {
      setSelection(undefined);
      return;
    }
    const rect = view.coordsAtPos(to) ?? view.coordsAtPos(from);
    if (rect === null) {
      return;
    }
    const quote = view.state.sliceDoc(from, to);
    setSelection({
      artifact: artifact.id,
      artifactId: artifact.id,
      canSuggest: true,
      from,
      occurrence: quoteOccurrence(view.state.doc.toString(), quote, from),
      quote,
      rect: { bottom: rect.bottom, left: rect.left, right: rect.right, top: rect.top },
      to,
    });
  };
  const queryClient = useQueryClient();
  const [connection, setConnection] = useState<ConnectionState>("connecting");
  const [content, setContent] = useState("");
  const [synced, setSynced] = useState(false);
  const [mode, setMode] = useState<EditorMode>("preview");
  const [localSelectedVersion, setLocalSelectedVersion] = useState<number | null>(null);
  const selectedVersion =
    onVersionChange === undefined ? localSelectedVersion : (controlledSelectedVersion ?? null);
  const setSelectedVersion = (version: number | null) => {
    if (onVersionChange === undefined) {
      setLocalSelectedVersion(version);
      return;
    }
    onVersionChange(version);
  };
  const [showDiff, setShowDiff] = useState(false);

  const artifactQuery = useQuery({
    queryKey: ["artifact", artifact.id],
    queryFn: () => api.getArtifact(artifact.id),
  });
  const liveTextQuery = useQuery({
    queryKey: ["artifact", artifact.id, "text"],
    queryFn: () => api.getArtifactText(artifact.id),
  });
  const versionTextQuery = useQuery({
    enabled: selectedVersion !== null,
    queryKey: ["artifact", artifact.id, "version", selectedVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, selectedVersion ?? 0),
  });
  const comments = useQuery({
    enabled: issueKey !== undefined && selectedVersion !== null,
    queryKey: ["comments", issueKey, artifact.id, "version"],
    queryFn: () => api.listComments(issueKey ?? "", artifact.id),
  });
  const nameVersion = useMutation({
    mutationFn: (summary: string) => api.createArtifactVersion(artifact.id, { summary }),
    onSuccess: (version) => {
      queryClient.setQueryData<Artifact>(["artifact", artifact.id], (current) => {
        const source = current ?? artifact;
        return {
          ...source,
          versions: [...source.versions.filter(({ number }) => number !== version.number), version],
        };
      });
      setMode("preview");
      setSelectedVersion(version.number);
      setShowDiff(false);
    },
  });
  const liveMarkdown = synced ? content : (liveTextQuery.data?.markdown ?? content);
  const selectedMarkdown =
    versionTextQuery.data !== undefined && "markdown" in versionTextQuery.data
      ? versionTextQuery.data.markdown
      : undefined;
  const versions = [...(artifactQuery.data?.versions ?? artifact.versions)].sort(
    (left, right) => right.number - left.number
  );
  const selectedVersionMeta =
    selectedVersion === null
      ? undefined
      : versions.find((version) => version.number === selectedVersion);
  const selectedAnchor =
    selectedVersion === null
      ? undefined
      : comments.data?.find(
          (comment) =>
            comment.anchor?.artifact_id === artifact.id &&
            comment.anchor.version === selectedVersion &&
            comment.anchor.from === highlight?.from &&
            comment.anchor.to === highlight?.to
        )?.anchor;
  const selectedHighlight =
    selectedVersion === null || selectedMarkdown === undefined
      ? undefined
      : historicalHighlight(selectedMarkdown, highlight, selectedAnchor);

  useEffect(() => {
    const parent = host.current;
    if (parent === null) {
      return;
    }

    let mounted = true;
    const document = new Y.Doc();
    const provider = new HocuspocusProvider({
      document,
      name: artifact.id,
      onStatus: ({ status }) => {
        if (mounted) {
          setConnection(status === "disconnected" ? "offline" : status);
        }
      },
      onSynced: ({ state }) => {
        if (mounted) {
          setSynced(state);
          setContent(document.getText("content").toString());
        }
      },
      url: wsUrl(artifact.id),
    });
    const awareness = provider.awareness;
    if (awareness === null) {
      provider.destroy();
      document.destroy();
      throw new Error("Dispatch document provider did not create awareness.");
    }
    awareness.setLocalStateField("user", {
      color: colorForLogin(userLoginRef.current),
      name: userLoginRef.current,
    });
    const ytext = document.getText("content");
    const syncContent = () => {
      if (mounted) {
        setContent(ytext.toString());
      }
    };
    ytext.observe(syncContent);

    const readOnly = new Compartment();
    const theme = new Compartment();
    const darkMediaQuery = window.matchMedia("(prefers-color-scheme: dark)");
    // CodeMirror's caret and selection colors are only legible against a dark background once a
    // theme opts into `{ dark: true }` (its own base theme otherwise assumes a light page, even
    // though it never sets a background itself — see `@codemirror/view`'s `baseTheme`); this
    // compartment carries no other styling, the surrounding page supplies the actual colors.
    const view = new EditorView({
      parent,
      state: EditorState.create({
        extensions: [
          EditorView.contentAttributes.of({ "aria-label": "Document editor" }),
          markdown(),
          EditorView.lineWrapping,
          EditorView.theme({ "&": { minHeight: "24rem" } }),
          theme.of(EditorView.theme({}, { dark: darkMediaQuery.matches })),
          yCollab(ytext, awareness),
          anchorDecorationExtension({
            onHover: (id) => anchorHover.current(id),
            onSelect: (id) => anchorSelect.current(id),
          }),
          EditorView.updateListener.of((update) => {
            if (update.selectionSet && update.view.hasFocus) {
              selectionHandler.current(update.view);
            }
          }),
          readOnly.of(editorAccess(isClosedRef.current)),
        ],
      }),
    });
    const applyColorScheme = (event: MediaQueryListEvent) => {
      view.dispatch({
        effects: theme.reconfigure(EditorView.theme({}, { dark: event.matches })),
      });
    };
    darkMediaQuery.addEventListener("change", applyColorScheme);
    providerRef.current = provider;
    viewRef.current = view;
    readOnlyRef.current = readOnly;
    themeRef.current = theme;
    setConnection("connecting");
    setContent("");
    setSynced(false);

    return () => {
      mounted = false;
      ytext.unobserve(syncContent);
      darkMediaQuery.removeEventListener("change", applyColorScheme);
      view.destroy();
      provider.destroy();
      document.destroy();
      if (providerRef.current === provider) {
        providerRef.current = null;
      }
      if (viewRef.current === view) {
        viewRef.current = null;
      }
      if (readOnlyRef.current === readOnly) {
        readOnlyRef.current = null;
      }
      if (themeRef.current === theme) {
        themeRef.current = null;
      }
    };
  }, [artifact.id]);

  useEffect(() => {
    const awareness = providerRef.current?.awareness;
    if (awareness === null || awareness === undefined) {
      return;
    }

    awareness.setLocalStateField("user", {
      color: colorForLogin(user.login),
      name: user.login,
    });
  }, [user.login]);

  useEffect(() => {
    const view = viewRef.current;
    const readOnly = readOnlyRef.current;
    if (view === null || readOnly === null) {
      return;
    }

    view.dispatch({ effects: readOnly.reconfigure(editorAccess(isClosed)) });
  }, [isClosed]);

  useEffect(() => {
    const view = viewRef.current;
    if (view === null) {
      return;
    }
    const documentLength = content.length;
    const decorationAnchors = anchors.filter(({ anchor }) => anchor.artifact_id === artifact.id);
    const decorationKey = decorationAnchors
      .map(({ anchor, id }) => `${id}:${anchor.from}:${anchor.to}:${anchor.orphaned}`)
      .join("|");
    if (decorationKey === decoratedAnchorKey.current && !awaitingAnchorDocument.current) {
      return;
    }
    if (
      decorationAnchors.some(
        ({ anchor }) => !anchor.orphaned && (anchor.from < 0 || anchor.to > documentLength)
      )
    ) {
      awaitingAnchorDocument.current = true;
      return;
    }

    view.dispatch({
      effects: setAnchorDecorations.of(
        decorationAnchors.map(({ anchor, id }) => ({ anchor, id, selected: false }))
      ),
    });
    decoratedAnchorKey.current = decorationKey;
    awaitingAnchorDocument.current = false;
  }, [anchors, artifact.id, content]);

  useEffect(() => {
    const view = viewRef.current;
    if (view === null) {
      return;
    }
    view.dispatch({
      effects: setActiveAnchorIds.of(
        [hoveredItemId, selectedItemId].filter(
          (itemId): itemId is string => itemId !== undefined && itemId !== null
        )
      ),
    });
  }, [hoveredItemId, selectedItemId]);

  const selectVersion = (value: string) => {
    setSelectedVersion(value === "" ? null : Number(value));
    if (value !== "") {
      setMode("preview");
    }
    setShowDiff(false);
  };
  const requestNamedVersion = () => {
    const summary = window.prompt("What changed in this version?");
    if (summary?.trim()) {
      nameVersion.mutate(summary.trim());
    }
  };

  return (
    <section aria-label="Document editor" className="space-y-4">
      <div
        className={`flex flex-wrap items-center justify-between gap-3 rounded-lg border p-3 ${card}`}
      >
        <div className="flex flex-wrap items-center gap-2">
          <button
            className={`rounded border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            onClick={() => {
              setMode((current) => (current === "edit" ? "preview" : "edit"));
              setSelectedVersion(null);
              setShowDiff(false);
            }}
            type="button"
          >
            {mode === "edit" ? "Preview" : "Edit"}
          </button>
          <button
            className={`rounded border px-3 py-1.5 text-sm font-medium disabled:cursor-not-allowed ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            disabled={isClosed || nameVersion.isPending}
            onClick={requestNamedVersion}
            type="button"
          >
            Name version
          </button>
          {selectedVersion === null ? null : (
            <button
              className={`rounded border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={() => setShowDiff((visible) => !visible)}
              type="button"
            >
              {showDiff ? "Show version" : "Diff vs current"}
            </button>
          )}
        </div>
        <div className="flex min-w-0 max-w-full flex-wrap items-center gap-3">
          <span
            className={`rounded-full px-2 py-1 text-xs font-medium ${
              connection === "connected"
                ? `${statusConnected.bg} ${statusConnected.text}`
                : connection === "connecting"
                  ? `${statusConnecting.bg} ${statusConnecting.text}`
                  : `${statusOffline.bg} ${statusOffline.text}`
            }`}
            role="status"
          >
            {connection}
          </span>
          <label
            className={`flex min-w-0 items-center text-sm font-medium ${textSecondaryOnSurface}`}
          >
            Version
            <select
              className={`ml-2 min-w-0 max-w-56 truncate rounded px-2 py-1 font-normal ${inputClasses(true)}`}
              onChange={(event) => selectVersion(event.target.value)}
              value={selectedVersion ?? ""}
            >
              <option value="">Current</option>
              {versions.map((version: Version) => (
                <option key={version.number} value={version.number}>
                  Version {version.number}
                  {version.named && version.summary !== null ? ` — ${version.summary}` : ""}
                </option>
              ))}
            </select>
          </label>
        </div>
      </div>
      {isClosed ? (
        <p
          className={`rounded-lg border p-3 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
        >
          This issue is closed. Its document is read-only.
        </p>
      ) : null}
      {nameVersion.isError ? (
        <p className={`text-sm ${dangerText}`} role="alert">
          Could not name this version.
        </p>
      ) : null}
      <div
        className={mode === "edit" && selectedVersion === null ? "min-h-96" : "hidden"}
        ref={host}
      />
      {selectedVersion !== null && versionTextQuery.isError ? (
        <section aria-label={`Document version ${selectedVersion}`} className="space-y-3">
          <p className={`text-sm ${dangerText}`}>
            No version {selectedVersion} of {artifact.name}.
          </p>
          <Link
            className={`font-medium underline ${linkText} ${linkHoverText}`}
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
      ) : selectedVersion !== null && selectedMarkdown === undefined ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading version…</p>
      ) : selectedVersion !== null && selectedMarkdown !== undefined ? (
        showDiff ? (
          <VersionDiff after={liveMarkdown} before={selectedMarkdown} />
        ) : (
          <section aria-label={`Document version ${selectedVersion}`} className="space-y-3">
            <h2 className={`text-lg font-semibold ${textPrimaryOnSurface}`}>
              Version {selectedVersion}
              {selectedVersionMeta?.created_at === undefined ? null : (
                <span className={`ml-2 text-sm font-normal ${textMutedOnSurface}`}>
                  <Timestamp at={selectedVersionMeta.created_at} />
                </span>
              )}
            </h2>
            <div data-testid="version-view">
              <DocView highlight={selectedHighlight} markdown={selectedMarkdown} />
            </div>
          </section>
        )
      ) : mode === "preview" ? (
        <DocView
          highlight={highlight}
          markdown={liveMarkdown}
          onSelectionChange={(next) => {
            setSelection(
              next === undefined
                ? undefined
                : {
                    ...next,
                    artifact: artifact.id,
                    artifactId: artifact.id,
                    canSuggest: false,
                  }
            );
          }}
        />
      ) : null}
    </section>
  );
}
