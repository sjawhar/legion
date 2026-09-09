import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, Navigate, Route, Routes, useLocation } from "react-router-dom";

import { api } from "./api/client";
import { useEventStream } from "./api/sse";
import type { AuthenticatedUser } from "./api/types";
import { AskCard } from "./features/inbox/AskCard";
import { Inbox } from "./features/inbox/Inbox";
import { IssuePage } from "./features/issue/IssuePage";
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

function Margin(): ReactNode {
  const { pathname } = useLocation();
  const askId = pathname.match(/^\/issues\/[^/]+\/asks\/([^/]+)$/)?.[1];
  const commentId = pathname.match(/^\/issues\/[^/]+\/comments\/([^/]+)$/)?.[1];
  const ask = useQuery({
    enabled: askId !== undefined,
    queryKey: ["ask", askId],
    queryFn: () => api.getAsk(askId ?? ""),
  });

  return (
    <aside className="fixed inset-x-0 bottom-0 z-10 border-t border-slate-200 bg-white p-4 shadow-[0_-8px_24px_rgba(15,23,42,0.08)] md:static md:order-3 md:w-80 md:border-t-0 md:border-l md:shadow-none">
      {askId === undefined && commentId === undefined ? (
        <p className="text-sm text-slate-500">Select an ask or comment to review it here.</p>
      ) : askId !== undefined && ask.data !== undefined ? (
        <AskCard ask={ask.data} />
      ) : askId !== undefined && ask.isError ? (
        <p className="text-sm text-rose-700">Could not load this ask.</p>
      ) : (
        <div>
          <p className="text-sm font-medium text-sky-700">Comment</p>
          <p className="mt-1 break-all text-sm text-slate-600">{commentId}</p>
        </div>
      )}
    </aside>
  );
}

function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
  return (
    <div className="min-h-dvh bg-slate-50 text-slate-900 md:flex">
      <aside className="border-b border-slate-200 bg-slate-950 p-5 text-slate-100 md:order-1 md:min-h-dvh md:w-80 md:border-r md:border-b-0">
        <Link className="text-lg font-semibold" to="/">
          Dispatch
        </Link>
        <p className="mt-3 text-sm text-slate-400">Signed in as {user.login}</p>
        <Sidebar />
      </aside>
      <main className="min-w-0 flex-1 p-6 pb-32 md:order-2 md:pb-6">
        <Routes>
          <Route
            path="/"
            element={
              <section>
                <h1 className="mb-6 text-2xl font-semibold">Inbox</h1>
                <Inbox />
              </section>
            }
          />
          <Route path="/issues/:key/*" element={<IssuePage />} />
          <Route path="*" element={<Navigate replace to="/" />} />
        </Routes>
      </main>
      <Margin />
    </div>
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
