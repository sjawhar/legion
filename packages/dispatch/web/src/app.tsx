import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { Link, Route, Routes, useLocation } from "react-router-dom";

import { api } from "./api/client";
import { useEventStream } from "./api/sse";
import type { AuthenticatedUser } from "./api/types";
import { ArtifactsTab } from "./features/artifacts/ArtifactsTab";
import { Inbox } from "./features/inbox/Inbox";
import { IssuePage } from "./features/issue/IssuePage";
import { Margin, MarginProvider } from "./features/margin/Margin";
import { parseIssuePath } from "./features/refs/routes";
import { NotFoundPage } from "./features/shell/NotFoundPage";
import { useDialog } from "./features/shell/useDialog";
import { useDocumentTitle } from "./features/shell/useDocumentTitle";
import { Sidebar } from "./features/sidebar/Sidebar";

function SignInPage(): ReactNode {
  return (
    <main className="grid min-h-dvh place-items-center bg-slate-950 px-6 text-slate-100">
      <section className="w-full max-w-md rounded-2xl border border-slate-700 bg-slate-900 p-8 shadow-2xl">
        <p className="text-sm font-medium tracking-wide text-sky-300 uppercase">Dispatch</p>
        <h1 className="mt-3 text-3xl font-semibold">Make the next decision clear.</h1>
        <p className="mt-3 text-slate-300">Sign in to review your team&apos;s open decisions.</p>
        <a
          className="mt-8 inline-flex rounded-lg bg-sky-400 px-4 py-2 font-semibold text-slate-950 hover:bg-sky-300"
          href="/auth/start"
        >
          Sign in with GitHub
        </a>
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

function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
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
      <div className="min-h-dvh bg-slate-50 text-slate-900 md:flex">
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
          <Sidebar onNavigate={() => setNavigationOpen(false)} />
        </aside>
        <main
          className="min-w-0 flex-1 p-6 pb-32 outline-none md:order-2 md:pb-6"
          id="main-content"
          ref={mainRef}
          tabIndex={-1}
        >
          <Routes>
            <Route element={<InboxPage />} path="/" />
            <Route element={<IssuePage user={user} />} path="/issues/:key/*" />
            <Route element={<NotFoundPage />} path="*" />
          </Routes>
        </main>
        <Margin ArtifactsTabSlot={ArtifactsTab} />
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
    retry: false,
  });

  if (whoAmI.isPending) {
    return (
      <main className="grid min-h-dvh place-items-center text-slate-500">Loading Dispatch…</main>
    );
  }
  if (whoAmI.isError || whoAmI.data === undefined) {
    return <SignInPage />;
  }

  return <AuthenticatedApp user={whoAmI.data} />;
}

export function App(): ReactNode {
  return <AuthGate />;
}
