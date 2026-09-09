import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import {
  Link,
  Navigate,
  Route,
  Routes,
  useLocation,
  useParams,
  useSearchParams,
} from "react-router-dom";

import { api } from "./api/client";
import { useEventStream } from "./api/sse";
import type { AuthenticatedUser } from "./api/types";

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

function Inbox(): ReactNode {
  const inbox = useQuery({
    queryKey: ["inbox"],
    queryFn: () => api.getInbox(),
  });

  if (inbox.isPending) {
    return <p className="text-slate-500">Loading your inbox…</p>;
  }

  if (inbox.isError) {
    return <p className="text-rose-700">Could not load your inbox.</p>;
  }

  if (inbox.data.length === 0) {
    return (
      <p className="rounded-xl border border-dashed border-slate-300 p-8 text-slate-500">
        Nothing needs you
      </p>
    );
  }

  return (
    <ul className="space-y-3">
      {inbox.data.map((ask) => (
        <li key={ask.id}>
          <Link
            className="block rounded-xl border border-slate-200 p-4 hover:border-sky-400"
            to={`/issues/${ask.issue_key}/asks/${ask.id}`}
          >
            <p className="font-medium text-slate-950">{ask.question}</p>
            <p className="mt-1 text-sm text-slate-500">{ask.issue?.title ?? ask.issue_key}</p>
          </Link>
        </li>
      ))}
    </ul>
  );
}

function IssueScreen({ title }: { title: string }): ReactNode {
  const { key } = useParams();
  const [searchParams] = useSearchParams();
  const artifactVersion = searchParams.get("v");

  return (
    <section>
      <p className="text-sm font-medium text-sky-700">{key}</p>
      <h1 className="mt-1 text-2xl font-semibold text-slate-950">{title}</h1>
      {artifactVersion === null ? null : (
        <p className="mt-3 text-slate-500">Version {artifactVersion}</p>
      )}
    </section>
  );
}

function Margin(): ReactNode {
  const { pathname } = useLocation();
  const selected = pathname.match(/\/(asks|comments)\/([^/]+)$/);

  return (
    <aside className="fixed inset-x-0 bottom-0 z-10 border-t border-slate-200 bg-white p-4 shadow-[0_-8px_24px_rgba(15,23,42,0.08)] md:static md:order-3 md:w-80 md:border-t-0 md:border-l md:shadow-none">
      {selected === null ? (
        <p className="text-sm text-slate-500">Select an ask or comment to review it here.</p>
      ) : (
        <div>
          <p className="text-sm font-medium text-sky-700">
            {selected[1] === "asks" ? "Ask" : "Comment"}
          </p>
          <p className="mt-1 break-all text-sm text-slate-600">{selected[2]}</p>
        </div>
      )}
    </aside>
  );
}

function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
  return (
    <div className="min-h-dvh bg-slate-50 text-slate-900 md:flex">
      <aside className="border-b border-slate-200 bg-slate-950 p-5 text-slate-100 md:order-1 md:min-h-dvh md:w-64 md:border-r md:border-b-0">
        <Link className="text-lg font-semibold" to="/">
          Dispatch
        </Link>
        <p className="mt-8 text-sm text-slate-400">Signed in as {user.login}</p>
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
          <Route path="/issues/:key" element={<IssueScreen title="Issue" />} />
          <Route path="/issues/:key/spec" element={<IssueScreen title="Specification" />} />
          <Route path="/issues/:key/artifacts/:slug" element={<IssueScreen title="Artifact" />} />
          <Route path="/issues/:key/asks/:id" element={<IssueScreen title="Issue" />} />
          <Route path="/issues/:key/comments/:id" element={<IssueScreen title="Issue" />} />
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
