import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { lazy, type ReactNode, Suspense, useEffect, useRef, useState } from "react";
import { Link, Route, Routes, useLocation } from "react-router-dom";

import { api, isForbidden, isUnauthorized } from "./api/client";
import { useEventStream } from "./api/sse";
import type { AuthenticatedUser } from "./api/types";
import { Inbox } from "./features/inbox/Inbox";
import { Margin, MarginProvider } from "./features/margin/Margin";
import { parseIssuePath } from "./features/refs/routes";
import { NotFoundPage } from "./features/shell/NotFoundPage";
import { useDialog } from "./features/shell/useDialog";
import { useDocumentTitle } from "./features/shell/useDocumentTitle";
import { Sidebar } from "./features/sidebar/Sidebar";

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
        <div
          className="h-20 w-full animate-pulse rounded-lg bg-slate-100 dark:bg-slate-800"
          key={row}
        />
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
      <header className="mb-6 border-b border-slate-200 pb-6 dark:border-slate-800">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm font-semibold text-sky-700">
            <span className="inline-block h-4 w-16 animate-pulse rounded bg-slate-200 align-middle dark:bg-slate-800" />
          </p>
          <button
            className="rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-transparent dark:border-slate-700"
            disabled
            type="button"
          >
            Pin issue
          </button>
        </div>
        <h1 className="mt-2 w-full rounded-lg border border-transparent px-2 py-1 text-2xl font-semibold">
          <span className="inline-block h-7 w-2/3 animate-pulse rounded bg-slate-200 align-middle dark:bg-slate-800" />
        </h1>
      </header>
      <div
        className="mb-4 flex gap-2 border-b border-slate-200 dark:border-slate-800"
        role="tablist"
      >
        {["Spec", "Log", "Children"].map((label) => (
          <span
            className="animate-pulse rounded bg-slate-100 px-3 py-2 text-sm text-transparent dark:bg-slate-800"
            key={label}
          >
            {label}
          </span>
        ))}
      </div>
      <div className="space-y-3">
        {[0, 1, 2, 3].map((row) => (
          <div
            className="h-4 w-full animate-pulse rounded bg-slate-200 dark:bg-slate-800"
            key={row}
          />
        ))}
      </div>
    </div>
  );
}

function ShellMessagePage({ children, title }: { children?: ReactNode; title: string }): ReactNode {
  return (
    <main className="grid min-h-dvh place-items-center bg-slate-50 px-6 text-slate-900 dark:bg-slate-950 dark:text-slate-100">
      <section className="w-full max-w-md rounded-2xl border border-slate-200 bg-white p-8 shadow-sm dark:border-slate-800 dark:bg-slate-900">
        <p className="text-sm font-medium tracking-wide text-slate-500 uppercase dark:text-slate-400">
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
      <p className="mt-3 text-slate-600 dark:text-slate-300">
        Sign in to review your team&apos;s open decisions.
      </p>
      <a
        className="mt-8 inline-flex rounded-lg bg-sky-600 px-4 py-2 font-semibold text-white hover:bg-sky-500"
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
      <p className="mt-3 text-slate-600 dark:text-slate-300">
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
          className="text-sm font-medium text-sky-700 underline hover:text-sky-900 dark:text-sky-400"
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
    <div
      aria-busy="true"
      className="min-h-dvh bg-slate-50 text-slate-900 md:flex dark:bg-slate-950 dark:text-slate-100"
    >
      <header className="flex items-center border-b border-slate-200 bg-slate-950 px-3 text-slate-100 md:hidden">
        <span className="text-lg font-semibold">Dispatch</span>
      </header>
      <aside className="hidden border-slate-200 bg-slate-950 p-5 text-slate-100 md:block md:min-h-dvh md:w-80 md:border-r">
        <span className="text-lg font-semibold">Dispatch</span>
        <div aria-label="Loading navigation" className="mt-6 space-y-2" role="status">
          {[0, 1, 2, 3].map((row) => (
            <div className="h-4 w-full animate-pulse rounded bg-slate-800" key={row} />
          ))}
        </div>
      </aside>
      <main className="min-w-0 flex-1 p-6">
        <div aria-label="Loading Dispatch" className="space-y-3" role="status">
          {[0, 1, 2].map((row) => (
            <div
              className="h-4 w-full animate-pulse rounded bg-slate-200 dark:bg-slate-800"
              key={row}
            />
          ))}
        </div>
      </main>
    </div>
  );
}

function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
  const queryClient = useQueryClient();
  const [navigationOpen, setNavigationOpen] = useState(false);
  const location = useLocation();
  const mainRef = useRef<HTMLElement | null>(null);
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const isFirstRender = useRef(true);
  const drawer = useDialog<HTMLElement>({
    initialFocusRef: closeButtonRef,
    onClose: () => setNavigationOpen(false),
    open: navigationOpen,
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

  return (
    <MarginProvider>
      <div className="min-h-dvh bg-slate-50 text-slate-900 md:flex dark:bg-slate-950 dark:text-slate-100">
        <a
          className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-lg focus:bg-slate-950 focus:px-4 focus:py-2 focus:text-slate-100"
          href="#main-content"
        >
          Skip to content
        </a>
        <header className="flex items-center justify-between border-b border-slate-200 bg-slate-950 px-3 text-slate-100 md:hidden">
          <Link className="text-lg font-semibold" to="/">
            Dispatch
          </Link>
          <button
            aria-expanded={navigationOpen}
            aria-label="Open navigation"
            className="rounded-lg px-3 py-2 text-sm font-medium hover:bg-slate-800"
            onClick={() => setNavigationOpen(true)}
            type="button"
          >
            Menu
          </button>
        </header>
        {navigationOpen ? (
          <div
            aria-hidden="true"
            className="fixed inset-0 z-20 bg-slate-950/50 md:hidden"
            onClick={() => setNavigationOpen(false)}
          />
        ) : null}
        {/* biome-ignore lint/a11y/useAriaPropsSupportedByRole: role/aria-modal apply only while navigationOpen makes this a dialog */}
        <aside
          aria-label="Navigation"
          aria-modal={navigationOpen ? true : undefined}
          className={`${
            navigationOpen ? "fixed inset-y-0 left-0 z-30 w-80 max-w-[calc(100vw-2rem)]" : "hidden"
          } border-b border-slate-200 bg-slate-950 p-5 text-slate-100 shadow-2xl md:static md:order-1 md:block md:min-h-dvh md:w-80 md:max-w-none md:border-r md:border-b-0 md:shadow-none`}
          ref={drawer.containerRef}
          role={navigationOpen ? "dialog" : undefined}
        >
          <div className="flex items-center justify-between md:block">
            <Link className="text-lg font-semibold" onClick={() => setNavigationOpen(false)} to="/">
              Dispatch
            </Link>
            <button
              aria-label="Close navigation"
              className="rounded-lg px-3 py-2 text-sm font-medium hover:bg-slate-800 md:hidden"
              onClick={() => setNavigationOpen(false)}
              ref={closeButtonRef}
              type="button"
            >
              Close
            </button>
          </div>
          <p className="mt-3 text-sm text-slate-400">Signed in as {user.login}</p>
          <button
            className="mt-1 text-sm font-medium text-sky-300 hover:text-sky-200 disabled:cursor-not-allowed disabled:opacity-50"
            disabled={signOut.isPending}
            onClick={() => signOut.mutate()}
            type="button"
          >
            Sign out
          </button>
          {signOut.isError ? (
            <p className="mt-1 text-sm text-rose-400" role="alert">
              Couldn&apos;t sign out.{" "}
              <button
                className="font-medium underline"
                onClick={() => signOut.mutate()}
                type="button"
              >
                Retry
              </button>
            </p>
          ) : null}
          <Sidebar onNavigate={() => setNavigationOpen(false)} />
        </aside>
        <main
          className="min-w-0 flex-1 p-6 pb-32 outline-none md:order-2 md:pb-6"
          id="main-content"
          ref={mainRef}
          tabIndex={-1}
        >
          <Suspense fallback={<IssuePageFallback />}>
            <Routes>
              <Route element={<InboxPage />} path="/" />
              <Route element={<IssuePage user={user} />} path="/issues/:key/*" />
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
