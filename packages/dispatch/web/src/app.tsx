import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { lazy, type ReactNode, type RefObject, Suspense, useEffect, useRef, useState } from "react";
import { Link, Route, Routes, useLocation, useNavigate } from "react-router-dom";

import { api, isForbidden, isUnauthorized } from "./api/client";
import { useConnectionState } from "./api/live";
import { inboxQuery, userAgentStateQuery, whoAmIQuery } from "./api/queries";
import { useEventStream } from "./api/sse";
import type { AuthenticatedUser } from "./api/types";
import { totalUnreadReplies, unreadRepliesLabel } from "./features/agents/unread";
import { waitingOnYou } from "./features/inbox/BlockedOnYou";
import { Inbox } from "./features/inbox/Inbox";
import { CreateIssueDialog } from "./features/issue/CreateIssueDialog";
import { DEFAULT_MARGIN_WIDTH, Margin } from "./features/margin/Margin";
import { MarginProvider } from "./features/margin/margin-context";
import { RefPreviewHost } from "./features/refs/RefPreview";
import {
  AGENT_LIVE_PATH,
  buildProjectPath,
  parseIssuePath,
  parseProjectPath,
  routeFillsViewport,
  routeHasMargin,
  routeProjectOf,
} from "./features/refs/routes";
import { SearchButton } from "./features/search/SearchButton";
import { type PaletteMode, SearchPalette } from "./features/search/SearchPalette";
import { SettingsPage } from "./features/settings/SettingsPage";
import { ErrorBoundary } from "./features/shell/ErrorBoundary";
import { KeymapProvider } from "./features/shell/KeymapProvider";
import { appKeymap, type KeyBindingDescription, useKeymap } from "./features/shell/keymap";
import { NotFoundPage } from "./features/shell/NotFoundPage";
import { ShortcutHelp } from "./features/shell/ShortcutHelp";
import { COMPACT_VIEWPORT_QUERY, useDialog, useMediaQuery } from "./features/shell/useDialog";
import { useDocumentTitle } from "./features/shell/useDocumentTitle";
import { useKeyboardFit } from "./features/shell/useKeyboardFit";
import { useUserPreference } from "./features/shell/userPreference";
import { Sidebar } from "./features/sidebar/Sidebar";
import {
  backdrop50,
  badgeBlocking,
  borderDefault,
  borderStrong,
  borderTransparent,
  calloutDangerBg,
  calloutDangerBorder,
  calloutWarningBorder,
  canvasText,
  card,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonHoverBg,
  railAccentHoverText,
  railAccentText,
  railActiveBg,
  railBg,
  railBorder,
  railDangerText,
  railFocusOverlayBg,
  railFocusOverlayText,
  railHoverBg,
  railMutedText,
  railNeedsYouBadgeBg,
  railNeedsYouBadgeText,
  railText,
  skeletonBg,
  statusConnecting,
  textMutedOnSurface,
  textSecondaryOnSurface,
  textTransparent,
} from "./theme/classes";

const IssuePage = lazy(() =>
  import("./features/issue/IssuePage").then((module) => ({ default: module.IssuePage }))
);

const ProjectPage = lazy(() =>
  import("./features/project/ProjectPage").then((module) => ({ default: module.ProjectPage }))
);
const DocumentPage = lazy(() =>
  import("./features/document/DocumentPage").then((module) => ({ default: module.DocumentPage }))
);

const AgentsPage = lazy(() =>
  import("./features/agents/AgentsPage").then((module) => ({ default: module.AgentsPage }))
);

const AgentConversationPage = lazy(() =>
  import("./features/agent-view/AgentConversationPage").then((module) => ({
    default: module.AgentConversationPage,
  }))
);

const BroadcastsPage = lazy(() =>
  import("./features/agents/BroadcastsPage").then((module) => ({ default: module.BroadcastsPage }))
);

const BroadcastPage = lazy(() =>
  import("./features/agents/BroadcastPage").then((module) => ({ default: module.BroadcastPage }))
);

const CredentialRecordPage = lazy(() =>
  import("./features/credentials/CredentialRecordPage").then((module) => ({
    default: module.CredentialRecordPage,
  }))
);
const MachineLoginPage = lazy(() =>
  import("./features/credentials/MachineLoginPage").then((module) => ({
    default: module.MachineLoginPage,
  }))
);

function IssuePageFallback(): ReactNode {
  return (
    <div aria-busy="true">
      <header className={`mb-6 border-b pb-6 ${borderDefault}`}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className={`text-sm font-semibold ${linkText}`}>
            <span
              className={`inline-block h-4 w-16 animate-pulse rounded align-middle ${skeletonBg}`}
            />
          </p>
          <button
            className={`rounded-lg border px-3 py-2 text-sm font-medium ${textTransparent} ${borderStrong}`}
            disabled
            type="button"
          >
            Pin issue
          </button>
        </div>
        <h1
          className={`mt-2 w-full rounded-lg border px-2 py-1 text-2xl font-semibold ${borderTransparent}`}
        >
          <span
            className={`inline-block h-7 w-2/3 animate-pulse rounded align-middle ${skeletonBg}`}
          />
        </h1>
      </header>
      <div className={`mb-4 flex gap-2 border-b ${borderDefault}`} role="tablist">
        {["Spec", "Conversation", "Children", "Artifacts"].map((label) => (
          <span
            className={`animate-pulse rounded px-3 py-2 text-sm ${textTransparent} ${skeletonBg}`}
            key={label}
          >
            {label}
          </span>
        ))}
      </div>
      <div className="space-y-3">
        {[0, 1, 2, 3].map((row) => (
          <div className={`h-4 w-full animate-pulse rounded ${skeletonBg}`} key={row} />
        ))}
      </div>
    </div>
  );
}

function ShellMessagePage({ children, title }: { children?: ReactNode; title: string }): ReactNode {
  return (
    <main className={`grid min-h-dvh place-items-center px-6 ${canvasText}`}>
      <section className={`w-full max-w-md rounded-2xl p-8 shadow-sm ${card}`}>
        <p className={`text-sm font-medium tracking-wide uppercase ${textMutedOnSurface}`}>
          Dispatch
        </p>
        <h1 className="mt-3 text-2xl font-semibold">{title}</h1>
        {children}
      </section>
    </main>
  );
}

function InboxPage(): ReactNode {
  useDocumentTitle("Inbox · Dispatch");
  return (
    <section>
      <h1 className="mb-4 text-[22px] font-semibold tracking-tight">Inbox</h1>
      <Inbox />
    </section>
  );
}

function SignInPage(): ReactNode {
  return (
    <ShellMessagePage title="Make the next decision clear.">
      <p className={`mt-3 ${textSecondaryOnSurface}`}>
        Sign in to review your team&apos;s open decisions.
      </p>
      <a
        className={`mt-8 inline-flex rounded-lg px-4 py-2 font-semibold ${primaryButtonBg} ${primaryButtonHoverBg}`}
        href="/auth/start"
      >
        Sign in with Google
      </a>
    </ShellMessagePage>
  );
}

function ForbiddenPage(): ReactNode {
  return (
    <ShellMessagePage title="This GitHub account isn't allowed here.">
      <p className={`mt-3 ${textSecondaryOnSurface}`}>
        Ask a Dispatch admin to add your account, then sign in again.
      </p>
    </ShellMessagePage>
  );
}

function ConnectionErrorPage({ onRetry }: { onRetry: () => void }): ReactNode {
  return (
    <ShellMessagePage title="Couldn't reach Dispatch.">
      <div className="mt-4" role="alert">
        <button
          className={`text-sm font-medium underline ${linkText} ${linkHoverText}`}
          onClick={onRetry}
          type="button"
        >
          Retry
        </button>
      </div>
    </ShellMessagePage>
  );
}

function ShellSkeleton(): ReactNode {
  return (
    <div aria-busy="true" className={`xl:flex ${canvasText}`}>
      <header
        className={`flex items-center px-3 xl:hidden ${railBorder} ${railBg} ${railText} border-b`}
      >
        <span className="text-lg font-semibold">Dispatch</span>
      </header>
      <aside
        className={`hidden p-5 xl:block xl:min-h-dvh xl:w-80 xl:border-r ${railBorder} ${railBg} ${railText}`}
      >
        <span className="text-lg font-semibold">Dispatch</span>
        <div aria-label="Loading navigation" className="mt-6 space-y-2" role="status">
          {[0, 1, 2, 3].map((row) => (
            <div className={`h-4 w-full animate-pulse rounded ${railActiveBg}`} key={row} />
          ))}
        </div>
      </aside>
      <main className="min-w-0 flex-1 p-6">
        <div aria-label="Loading Dispatch" className="space-y-3" role="status">
          {[0, 1, 2].map((row) => (
            <div className={`h-4 w-full animate-pulse rounded ${skeletonBg}`} key={row} />
          ))}
        </div>
      </main>
    </div>
  );
}

function NavigationContents({
  closeButtonRef,
  compact,
  onClose,
  onHideSidebar,
  onSearch,
  onSignOut,
  signOutError,
  signOutPending,
  user,
}: {
  closeButtonRef: RefObject<HTMLButtonElement | null>;
  compact: boolean;
  onClose: () => void;
  onHideSidebar: () => void;
  onSearch: () => void;
  onSignOut: () => void;
  signOutError: boolean;
  signOutPending: boolean;
  user: AuthenticatedUser;
}): ReactNode {
  return (
    <>
      <div className="flex items-center justify-between">
        <Link className="text-lg font-semibold" onClick={onClose} to="/">
          Dispatch
        </Link>
        {compact ? (
          <button
            aria-label="Close navigation"
            className={`rounded-lg px-3 py-2 text-sm font-medium ${railHoverBg}`}
            onClick={onClose}
            ref={closeButtonRef}
            type="button"
          >
            Close
          </button>
        ) : null}
      </div>
      <SearchButton
        onOpen={() => {
          onClose();
          onSearch();
        }}
      />
      <Sidebar onHide={compact ? undefined : onHideSidebar} onNavigate={onClose} user={user} />
      {/* Identity is chrome: who you are and how to leave are read once, while the navigation
          above is read on every visit, so the footer sits under it rather than over it. */}
      <div className={`mt-auto border-t pt-4 ${railBorder}`}>
        <p className={`px-2 text-sm ${railMutedText}`}>Signed in as {user.login}</p>
        <button
          className={`mt-1 px-2 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50 ${railAccentText} ${railAccentHoverText}`}
          disabled={signOutPending}
          onClick={onSignOut}
          type="button"
        >
          Sign out
        </button>
        {signOutError ? (
          <p className={`mt-1 px-2 text-sm ${railDangerText}`} role="alert">
            Couldn&apos;t sign out.{" "}
            <button className="font-medium underline" onClick={onSignOut} type="button">
              Retry
            </button>
          </p>
        ) : null}
      </div>
    </>
  );
}

function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
  const queryClient = useQueryClient();
  const [navigationOpen, setNavigationOpen] = useState(false);
  // Which palette is open, and `null` for none: `$mod+k` and the rail's Search control list this
  // page's actions and the hits, `/` searches only, and `g p` lists projects.
  const [paletteMode, setPaletteMode] = useState<PaletteMode | null>(null);
  const [sidebarHidden, setSidebarHidden] = useUserPreference(
    "shell.sidebar",
    (stored) => stored === "hidden",
    (hidden) => (hidden ? "hidden" : "shown")
  );
  const [marginHidden, setMarginHidden] = useUserPreference(
    "shell.margin",
    (stored) => stored === "hidden",
    (hidden) => (hidden ? "hidden" : "shown")
  );
  const [marginWidth, setMarginWidth] = useUserPreference(
    "shell.margin-width",
    (storedWidth) => {
      const parsedWidth = Number(storedWidth);
      return storedWidth !== null && Number.isInteger(parsedWidth)
        ? parsedWidth
        : DEFAULT_MARGIN_WIDTH;
    },
    String
  );
  const location = useLocation();
  // The shell reserves a gutter for a rail only where that rail exists. The collapsed-margin
  // rail exists only where the route has a margin, so it is reserved from exactly the answer
  // `features/margin/Margin.tsx` renders from; two answers that disagree leave 80 px of
  // padding beside a rail that is not there. "Full width" means no expanded column on either
  // side - on a route with no margin, a hidden sidebar is enough.
  const hasMargin = routeHasMargin(location.pathname, location.search);
  const marginRailShown = hasMargin && marginHidden;
  const marginColumnShown = hasMargin && !marginHidden;
  const fullWidth = sidebarHidden && !marginColumnShown;
  const mainLayoutClass =
    sidebarHidden && marginRailShown
      ? "xl:w-full xl:pl-20 xl:pr-20"
      : sidebarHidden && !hasMargin
        ? "xl:w-full xl:pl-20"
        : sidebarHidden
          ? "xl:pl-20"
          : marginRailShown
            ? "xl:pr-20"
            : "";
  // A page that scrolls inside itself gets a viewport-tall shell (dynamic viewport units where
  // the browser has them, so a phone's collapsing toolbar is accounted for), with `<main>` a
  // flex column that hands it the height left below the compact header, capped above an iOS
  // on-screen keyboard while its composer has focus.
  const fillsViewport = routeFillsViewport(location.pathname);
  const connection = useConnectionState();
  const inbox = useQuery(inboxQuery());
  const needsYouCount = inbox.data === undefined ? 0 : waitingOnYou(inbox.data).length;
  const unreadReplies = totalUnreadReplies(useQuery(userAgentStateQuery()).data);

  const mainRef = useRef<HTMLElement | null>(null);
  useKeyboardFit(mainRef, fillsViewport);
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const isFirstRender = useRef(true);
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const drawer = useDialog<HTMLElement>({
    initialFocusRef: closeButtonRef,
    onClose: () => setNavigationOpen(false),
    open: navigationOpen && isCompactViewport,
  });
  const navigate = useNavigate();
  // The registry as described when `?` fired (focus still on the caller); `null` while closed.
  const [helpSnapshot, setHelpSnapshot] = useState<readonly KeyBindingDescription[] | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const routeProject = routeProjectOf(location.pathname);
  useKeymap("global", [
    {
      // Opens only: while the palette is open the `dialog` scope is the only one consulted, and
      // the palette's own `$mod+k` binding closes it.
      id: "search",
      inEditable: true,
      keys: "$mod+k",
      label: "Search and actions",
      run: () => setPaletteMode("all"),
    },
    {
      // No row: chosen from `$mod+k`'s palette it would only reopen that palette with its actions
      // taken away.
      id: "search-only",
      keys: "/",
      label: "Search only",
      palette: false,
      run: () => setPaletteMode("search"),
    },
    {
      id: "help",
      keys: "?",
      label: "Keyboard shortcuts",
      run: () => setHelpSnapshot(appKeymap.describe()),
    },
    { id: "create", keys: "c", label: "Create issue", run: () => setCreateOpen(true) },
    { id: "go-inbox", keys: "g i", label: "Go to Inbox", run: () => navigate("/") },
    { id: "go-agents", keys: "g a", label: "Go to Agents", run: () => navigate("/agents") },
    { id: "go-settings", keys: "g s", label: "Go to Settings", run: () => navigate("/settings") },
    {
      id: "go-documents",
      keys: "g d",
      label: "Go to Documents",
      run: () => {
        if (routeProject !== undefined) {
          navigate(buildProjectPath({ kind: "documents", project: routeProject }));
        }
      },
      when: () => routeProject !== undefined,
    },
    {
      id: "go-project",
      keys: "g p",
      label: "Go to project…",
      run: () => setPaletteMode("projects"),
    },
    {
      id: "toggle-sidebar",
      keys: "Shift+S",
      label: "Toggle sidebar",
      run: () => setSidebarHidden(!sidebarHidden),
      // Below `xl` the sidebar is a sheet with its own Menu control, and the preference is inert.
      when: () => !isCompactViewport,
    },
    {
      id: "toggle-margin",
      keys: "Shift+M",
      label: "Toggle margin",
      run: () => setMarginHidden(!marginHidden),
      // Below `xl` the margin is a sheet the route opens itself: `Margin` reads the preference
      // only from `xl`, so a toggle there would change nothing on screen and flip it for later.
      when: () => !isCompactViewport && hasMargin,
    },
  ]);
  const signOut = useMutation({
    mutationFn: () => api.logout(),
    onSuccess: () => {
      // `resetQueries` (not `clear`) forces the active `["whoami"]` observer in AuthGate to
      // refetch immediately — a signed-out user must land on sign-in without a manual reload.
      void queryClient.resetQueries({ queryKey: ["whoami"] });
      queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== "whoami" });
    },
  });
  // Tabs within the same issue manage their own focus (roving tabindex); only a
  // genuine page change — a different issue, or a different top-level route —
  // should move focus to the main region.
  const pageIdentity =
    parseIssuePath(location.pathname)?.key ??
    parseProjectPath(location.pathname)?.project ??
    location.pathname;

  // biome-ignore lint/correctness/useExhaustiveDependencies: re-run only to move focus to main on a real page change
  useEffect(() => {
    if (isFirstRender.current) {
      isFirstRender.current = false;
      return;
    }
    mainRef.current?.focus();
  }, [pageIdentity]);

  const navigation = (
    <NavigationContents
      closeButtonRef={closeButtonRef}
      compact={isCompactViewport}
      onClose={() => setNavigationOpen(false)}
      onHideSidebar={() => setSidebarHidden(true)}
      onSearch={() => setPaletteMode("all")}
      onSignOut={() => signOut.mutate()}
      signOutError={signOut.isError}
      signOutPending={signOut.isPending}
      user={user}
    />
  );

  return (
    <MarginProvider>
      <div
        className={`${fillsViewport ? "flex h-screen flex-col supports-[height:100dvh]:h-dvh xl:flex-row" : "xl:flex"} ${canvasText}`}
        data-testid="app-shell"
      >
        <a
          className={`sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-lg focus:px-4 focus:py-2 ${railFocusOverlayBg} ${railFocusOverlayText}`}
          href="#main-content"
        >
          Skip to content
        </a>
        {connection === "reconnecting" ? (
          <p
            aria-live="polite"
            className={`fixed right-4 bottom-20 z-40 rounded-full border px-3 py-1 text-xs font-medium shadow-lg xl:bottom-4 ${calloutWarningBorder} ${statusConnecting.bg} ${statusConnecting.text}`}
            data-testid="connection-pill"
          >
            Reconnecting…
          </p>
        ) : connection === "unavailable" ? (
          <p
            aria-live="polite"
            className={`fixed right-4 bottom-20 z-40 flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium shadow-lg xl:bottom-4 ${calloutDangerBorder} ${calloutDangerBg} ${badgeBlocking.text}`}
            data-testid="connection-pill"
          >
            Live updates unavailable
            <button className="underline" onClick={() => window.location.reload()} type="button">
              Reload
            </button>
          </p>
        ) : null}
        {isCompactViewport ? (
          <header
            className={`flex items-center justify-between border-b px-3 ${railBorder} ${railBg} ${railText}`}
          >
            <div className="flex items-center gap-2">
              <Link className="text-lg font-semibold" to="/">
                Dispatch
              </Link>
              {needsYouCount === 0 ? null : (
                <span
                  className={`rounded-full px-2 py-1 text-xs font-semibold ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
                >
                  Needs you {needsYouCount}
                </span>
              )}
              {unreadReplies === 0 ? null : (
                <Link
                  className={`rounded-full px-2 py-1 text-xs font-semibold ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
                  to="/agents"
                >
                  {unreadRepliesLabel(unreadReplies)}
                </Link>
              )}
            </div>
            <button
              aria-expanded={navigationOpen}
              aria-label="Open navigation"
              className={`rounded-lg px-3 py-2 text-sm font-medium ${railHoverBg}`}
              onClick={() => setNavigationOpen(true)}
              type="button"
            >
              Menu
            </button>
          </header>
        ) : null}
        {isCompactViewport && navigationOpen ? (
          <>
            <div
              aria-hidden="true"
              className={`fixed inset-0 z-20 ${backdrop50}`}
              onClick={() => setNavigationOpen(false)}
            />
            <aside
              aria-label="Navigation"
              aria-modal="true"
              className={`fixed inset-y-0 left-0 z-30 flex w-80 max-w-[calc(100vw-2rem)] flex-col overflow-y-auto border-b p-5 shadow-2xl ${railBorder} ${railBg} ${railText}`}
              ref={drawer.containerRef}
              role="dialog"
            >
              {navigation}
            </aside>
          </>
        ) : null}
        {isCompactViewport ? null : sidebarHidden ? (
          <aside
            aria-label="Collapsed sidebar"
            className={`fixed inset-y-0 left-0 z-10 flex w-14 justify-center border-r p-2 ${railBorder} ${railBg} ${railText}`}
            data-testid="sidebar-rail"
          >
            <button
              aria-label="Show sidebar"
              className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
              onClick={() => setSidebarHidden(false)}
              type="button"
            >
              <span aria-hidden="true">›</span>
            </button>
          </aside>
        ) : (
          // The sidebar is first in the document and as tall as the viewport, so left to itself
          // Chromium's scroll anchoring picks it as the anchor node - and since it never moves,
          // nothing is ever compensated. Excluding it makes the browser anchor inside <main>, so
          // content growing above the reader (a card's thread loading, Markdown rendering) no
          // longer shifts what they are looking at.
          <aside
            aria-label="Navigation"
            className={`relative order-1 w-80 max-w-none border-r [overflow-anchor:none] ${railBorder} ${railBg} ${railText}`}
          >
            {/* The aside stretches to the page's height, so the navigation is a viewport-tall
                column that sticks inside it: on a long issue the links stay reachable, and the
                identity footer sits at the bottom of the screen rather than the bottom of the
                document. */}
            <div className="sticky top-0 flex h-dvh flex-col overflow-y-auto p-5">{navigation}</div>
          </aside>
        )}
        <main
          className={`min-w-0 flex-1 p-6 outline-none xl:order-2 ${fillsViewport ? "flex min-h-0 flex-col" : "pb-32 xl:pb-6"} ${mainLayoutClass}`}
          data-shell-layout={fullWidth ? "full-width" : "standard"}
          data-testid="main-content"
          id="main-content"
          ref={mainRef}
          tabIndex={-1}
        >
          <ErrorBoundary region="this page" resetKey={location.pathname}>
            <Suspense fallback={<IssuePageFallback />}>
              <Routes>
                <Route element={<InboxPage />} path="/" />
                <Route element={<AgentsPage />} path="/agents" />
                <Route element={<BroadcastsPage />} path="/agents/broadcasts" />
                <Route element={<BroadcastPage />} path="/agents/broadcasts/:id" />
                <Route element={<AgentConversationPage />} path={AGENT_LIVE_PATH} />
                <Route element={<IssuePage />} path="/issues/:key/*" />
                <Route element={<ProjectPage />} path="/projects/:key" />
                <Route element={<ProjectPage />} path="/projects/:key/architecture" />
                <Route element={<ProjectPage />} path="/projects/:key/issues" />
                <Route element={<ProjectPage />} path="/projects/:key/documents" />
                <Route element={<DocumentPage />} path="/projects/:key/documents/:slug" />
                <Route element={<CredentialRecordPage />} path="/credentials/:recordId" />
                <Route element={<MachineLoginPage />} path="/credentials/machine" />
                <Route element={<SettingsPage />} path="/settings" />
                <Route element={<NotFoundPage />} path="*" />
              </Routes>
            </Suspense>
          </ErrorBoundary>
        </main>
        <ErrorBoundary region="the margin" resetKey={location.pathname}>
          <Margin
            collapsed={marginHidden}
            onCollapsedChange={setMarginHidden}
            onWidthChange={setMarginWidth}
            width={marginWidth}
          />
        </ErrorBoundary>
        <SearchPalette mode={paletteMode} onClose={() => setPaletteMode(null)} />
        <ShortcutHelp onClose={() => setHelpSnapshot(null)} snapshot={helpSnapshot} />
        {createOpen ? <CreateIssueDialog onClose={() => setCreateOpen(false)} /> : null}
        <RefPreviewHost />
      </div>
    </MarginProvider>
  );
}

function AuthenticatedApp({ user }: { user: AuthenticatedUser }): ReactNode {
  useEventStream();
  return (
    <KeymapProvider>
      <AppShell user={user} />
    </KeymapProvider>
  );
}

export function AuthGate(): ReactNode {
  const whoAmI = useQuery(whoAmIQuery());

  if (whoAmI.isPending) {
    return <ShellSkeleton />;
  }
  // A definitive auth outcome always wins, even over stale "signed in" data from before a
  // session expired or was revoked — otherwise a revoked user keeps the authenticated shell.
  if (whoAmI.isError && isUnauthorized(whoAmI.error)) {
    return <SignInPage />;
  }
  if (whoAmI.isError && isForbidden(whoAmI.error)) {
    return <ForbiddenPage />;
  }
  if (whoAmI.data !== undefined) {
    return <AuthenticatedApp user={whoAmI.data} />;
  }
  return <ConnectionErrorPage onRetry={() => void whoAmI.refetch()} />;
}

export function App(): ReactNode {
  return <AuthGate />;
}
