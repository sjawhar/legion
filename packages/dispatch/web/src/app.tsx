import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { lazy, type ReactNode, type RefObject, Suspense, useEffect, useRef, useState } from "react";
import { Link, Route, Routes, useLocation } from "react-router-dom";

import { api, isForbidden, isUnauthorized } from "./api/client";
import { useConnectionState } from "./api/live";
import { useEventStream } from "./api/sse";
import type { AuthenticatedUser } from "./api/types";
import { Inbox } from "./features/inbox/Inbox";
import { Margin, MarginProvider } from "./features/margin/Margin";
import { parseIssuePath } from "./features/refs/routes";
import { SettingsPage } from "./features/settings/SettingsPage";
import { NotFoundPage } from "./features/shell/NotFoundPage";
import { useDialog, useMediaQuery } from "./features/shell/useDialog";
import { useDocumentTitle } from "./features/shell/useDocumentTitle";
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
const LazyArtifactsTab = lazy(() =>
  import("./features/artifacts/ArtifactsTab").then((module) => ({ default: module.ArtifactsTab }))
);

function ArtifactsTabFallback(): ReactNode {
  return (
    <div aria-busy="true" className="space-y-3 py-4">
      {[0, 1].map((row) => (
        <div className={`h-20 w-full animate-pulse rounded-lg ${skeletonBg}`} key={row} />
      ))}
    </div>
  );
}

function ArtifactsTabSlot(): ReactNode {
  return (
    <Suspense fallback={<ArtifactsTabFallback />}>
      <LazyArtifactsTab />
    </Suspense>
  );
}

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
        {["Spec", "Log", "Children"].map((label) => (
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
      <h1 className="mb-6 text-2xl font-semibold">Inbox</h1>
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
        Sign in with GitHub
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
  onSignOut,
  signOutError,
  signOutPending,
  user,
}: {
  closeButtonRef: RefObject<HTMLButtonElement | null>;
  compact: boolean;
  onClose: () => void;
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
      <p className={`mt-3 text-sm ${railMutedText}`}>Signed in as {user.login}</p>
      <button
        className={`mt-1 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50 ${railAccentText} ${railAccentHoverText}`}
        disabled={signOutPending}
        onClick={onSignOut}
        type="button"
      >
        Sign out
      </button>
      {signOutError ? (
        <p className={`mt-1 text-sm ${railDangerText}`} role="alert">
          Couldn&apos;t sign out.{" "}
          <button className="font-medium underline" onClick={onSignOut} type="button">
            Retry
          </button>
        </p>
      ) : null}
      <Sidebar onNavigate={onClose} user={user} />
    </>
  );
}

function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
  const queryClient = useQueryClient();
  const [navigationOpen, setNavigationOpen] = useState(false);
  const connection = useConnectionState();
  const location = useLocation();
  const mainRef = useRef<HTMLElement | null>(null);
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const isFirstRender = useRef(true);
  const isCompactViewport = useMediaQuery("(max-width: 1279px)");
  const drawer = useDialog<HTMLElement>({
    initialFocusRef: closeButtonRef,
    onClose: () => setNavigationOpen(false),
    open: navigationOpen && isCompactViewport,
  });
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
  const pageIdentity = parseIssuePath(location.pathname)?.key ?? location.pathname;

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
      onSignOut={() => signOut.mutate()}
      signOutError={signOut.isError}
      signOutPending={signOut.isPending}
      user={user}
    />
  );

  return (
    <MarginProvider>
      <div className={`xl:flex ${canvasText}`} data-testid="app-shell">
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
            <Link className="text-lg font-semibold" to="/">
              Dispatch
            </Link>
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
              className={`fixed inset-y-0 left-0 z-30 w-80 max-w-[calc(100vw-2rem)] border-b p-5 shadow-2xl ${railBorder} ${railBg} ${railText}`}
              ref={drawer.containerRef}
              role="dialog"
            >
              {navigation}
            </aside>
          </>
        ) : null}
        {isCompactViewport ? null : (
          <aside
            aria-label="Navigation"
            className={`order-1 min-h-dvh w-80 max-w-none border-r p-5 ${railBorder} ${railBg} ${railText}`}
          >
            {navigation}
          </aside>
        )}
        <main
          className="min-w-0 flex-1 p-6 pb-32 outline-none xl:order-2 xl:pb-6"
          id="main-content"
          ref={mainRef}
          tabIndex={-1}
        >
          <Suspense fallback={<IssuePageFallback />}>
            <Routes>
              <Route element={<InboxPage />} path="/" />
              <Route element={<IssuePage user={user} />} path="/issues/:key/*" />
              <Route element={<SettingsPage />} path="/settings" />
              <Route element={<NotFoundPage />} path="*" />
            </Routes>
          </Suspense>
        </main>
        <Margin ArtifactsTabSlot={ArtifactsTabSlot} />
      </div>
    </MarginProvider>
  );
}

function AuthenticatedApp({ user }: { user: AuthenticatedUser }): ReactNode {
  useEventStream();
  return <AppShell user={user} />;
}

export function AuthGate(): ReactNode {
  const whoAmI = useQuery({
    queryKey: ["whoami"],
    queryFn: () => api.whoAmI(),
  });

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
