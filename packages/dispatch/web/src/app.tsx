import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, Navigate, Route, Routes } from "react-router-dom";

import { api } from "./api/client";
import { useEventStream } from "./api/sse";
import type { AuthenticatedUser } from "./api/types";
import { Inbox } from "./features/inbox/Inbox";
import { IssuePage } from "./features/issue/IssuePage";
import { Margin, MarginProvider } from "./features/margin/Margin";
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

function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
  return (
    <MarginProvider>
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
            <Route path="/issues/:key/*" element={<IssuePage user={user} />} />
            <Route path="*" element={<Navigate replace to="/" />} />
          </Routes>
        </main>
        <Margin />
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
