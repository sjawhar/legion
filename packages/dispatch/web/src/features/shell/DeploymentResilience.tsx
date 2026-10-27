import { type ReactNode, useEffect, useRef, useState } from "react";

import {
  borderStrong,
  card,
  dismissButtonText,
  focusVisibleRing,
  primaryButtonBg,
  primaryButtonHoverBg,
  textSecondaryOnSurface,
} from "../../theme/classes";
import * as markdownEngine from "../refs/markdown-engine";

// The tab's recovery reloads, as a JSON array of the epoch-millisecond times they started: a chunk
// that failed to download reloads the page while fewer than CHUNK_RELOAD_BUDGET of them fall in the
// last CHUNK_RELOAD_WINDOW_MS. `web/index.html`'s inline script keeps the same rule on the same key
// for a script or stylesheet that failed before any of this bundle ran, so the two share one
// budget whichever failed first. The budget renews, so a tab open across many deployments recovers
// from each, and a page whose assets fail on every load still stops after three reloads rather than
// reloading for ever. The earlier once-only key, `dispatch.reloaded-for-chunk`, is neither read nor
// written: a page of an older build in the same tab keeps spending it on its own.
const CHUNK_RELOADS_STORAGE_KEY = "dispatch.chunk-reloads";
const CHUNK_RELOAD_BUDGET = 3;
const CHUNK_RELOAD_WINDOW_MS = 5 * 60_000;
const VERSION_CHECK_INTERVAL_MS = 60_000;
const INDEX_ASSET_PATH = /^\/assets\/index-[^/]+\.js$/;

declare const __DISPATCH_BUILD__: string;

// The Navigation API, as much of it as this module uses: TypeScript's DOM library does not declare
// it yet, and a browser without it leaves `window.navigation` undefined.
interface NavigateEvent extends Event {
  readonly destination: { readonly sameDocument: boolean; readonly url: string };
  readonly downloadRequest: string | null;
}

interface Navigation {
  addEventListener(type: "navigate", listener: (event: NavigateEvent) => void): void;
  removeEventListener(type: "navigate", listener: (event: NavigateEvent) => void): void;
}

declare global {
  interface Window {
    readonly navigation?: Navigation;
  }
}

function indexAssetPath(source: string): string | undefined {
  const pathname = source.startsWith("/") ? source.split(/[?#]/, 1)[0] : new URL(source).pathname;
  return INDEX_ASSET_PATH.test(pathname) ? pathname : undefined;
}

function runningIndexAsset(): string {
  const scripts = document.querySelectorAll<HTMLScriptElement>("script[src]");
  for (const script of scripts) {
    const asset = indexAssetPath(script.src);
    if (asset !== undefined) {
      return asset;
    }
  }
  // The build ID keeps Vite's entry chunk distinct for every deployment. The production index
  // script above is the normal comparison value; this fallback only applies outside that document.
  return __DISPATCH_BUILD__;
}

function newestIndexAsset(html: string): string | undefined {
  const scripts = html.matchAll(/<script\b[^>]*\bsrc=["']([^"']+)["'][^>]*>/gi);
  for (const script of scripts) {
    const asset = indexAssetPath(script[1]);
    if (asset !== undefined) {
      return asset;
    }
  }
  return undefined;
}

async function deploymentChanged(): Promise<boolean> {
  const health = await fetch("/healthz", { cache: "no-store" });
  if (!health.ok) {
    return false;
  }

  const index = await fetch("/", { cache: "no-store" });
  if (!index.ok) {
    return false;
  }

  const latest = newestIndexAsset(await index.text());
  return latest !== undefined && latest !== runningIndexAsset();
}

// Set from the start of a navigation away from this page (`beforeunload`, or the Navigation API's
// `navigate` to another document, since iOS Safari never fires `beforeunload`) until the page is
// plainly still the reader's: shown again (`pageshow`, which a back/forward-cache restore fires,
// or the tab becoming visible again), or pressed (a pointer or a key on it). WebKit and Firefox
// cancel the chunk downloads still in flight when a navigation starts, WebKit also refuses the ones
// the page starts after it, and Vite reports each as a failed chunk. Those are not a replaced
// deployment, and a reload then would replace the reader's navigation with a reload of the page
// they are leaving. A navigation can also not happen after all - cancelled at another page's leave
// prompt, stopped, or answered by the server with a download - and no event says so; the press or
// the return that follows is what ends the state, never a timer, since a refusal can arrive any
// number of tasks after the navigation started.
let leavingPage = false;

function markPageLeaving(): void {
  leavingPage = true;
}

// A route change inside the app is a navigation within this document. A link with `download`
// (Dispatch's Download version links) fires `navigate` to another document too, with
// `downloadRequest` set, and then downloads the file and leaves the page where it is. Firefox
// (Playwright's build, at least) then fires a second `navigate` for the same click, to the same
// URL with no `downloadRequest`, so that immediate navigation is skipped. Once the page is
// plainly still in use, the next navigation to that URL leaves like any other. iOS Safari fires
// `navigate` from 26.2, for a link followed or a form submitted but not for an address the reader
// types.
let lastDownloadUrl: string | undefined;

function markPageLeavingForAnotherDocument(event: NavigateEvent): void {
  if (event.destination.sameDocument) {
    return;
  }
  const download = lastDownloadUrl;
  lastDownloadUrl = undefined;
  if (event.downloadRequest !== null) {
    lastDownloadUrl = event.destination.url;
    return;
  }
  if (event.destination.url !== download) {
    leavingPage = true;
  }
}

function markPageStaying(): void {
  leavingPage = false;
  lastDownloadUrl = undefined;
}

function markPageStayingWhenVisible(): void {
  if (document.visibilityState === "visible") {
    markPageStaying();
  }
}

// The recovery reloads of this tab that started within the window before `now`. A value that does
// not parse as an array of times reads as none, and the next reload overwrites it.
function recentChunkReloads(now: number): number[] {
  let stored: unknown;
  try {
    stored = JSON.parse(window.sessionStorage.getItem(CHUNK_RELOADS_STORAGE_KEY) ?? "[]");
  } catch {
    return [];
  }
  if (!Array.isArray(stored)) {
    return [];
  }
  return stored.filter(
    (time): time is number => typeof time === "number" && now - time < CHUNK_RELOAD_WINDOW_MS
  );
}

/**
 * Spends one of the tab's recovery reloads at `now` and says whether one was left: true records
 * `now` (dropping the times that have left the window) and the caller reloads; false records
 * nothing. The same rule as `web/index.html`'s inline script, which shares the key.
 */
export function spendChunkReload(now: number): boolean {
  const recent = recentChunkReloads(now);
  if (recent.length >= CHUNK_RELOAD_BUDGET) {
    return false;
  }
  recent.push(now);
  window.sessionStorage.setItem(CHUNK_RELOADS_STORAGE_KEY, JSON.stringify(recent));
  return true;
}

// Reloads the page for a chunk that failed to download while online, from the tab's budget.
function reloadForChunkFailure(): void {
  if (leavingPage) {
    return;
  }
  // A chunk that fails to download while the browser is offline is a network outage, not a
  // replaced deployment: reloading now would swap the app for the browser's offline page.
  if (!navigator.onLine) {
    return;
  }
  if (!spendChunkReload(Date.now())) {
    return;
  }
  window.location.reload();
}

// Vite dispatches `vite:preloadError` for a chunk or one of its stylesheets that failed to
// download, and throws the failure to the importer unless the event is default-prevented. A
// prevented failure of the chunk itself resolves the import with `undefined`, which the importer
// reads as a module and fails on with a TypeError that says nothing about the download; a
// prevented stylesheet failure loads the module without its styles. So the handler never
// prevents it: the importer always sees the failure itself, and the page also reloads from the
// tab's budget, never while it is being left. A failed chunk or stylesheet Vite preloaded through a
// `<link>` is seen first by `web/index.html`'s inline script, which reloads; a reload fires
// `beforeunload` before it returns, so this event for the same failure finds the page being left
// and spends nothing more.
export function installChunkFailureRecovery(): () => void {
  const navigation = window.navigation;
  window.addEventListener("beforeunload", markPageLeaving);
  navigation?.addEventListener("navigate", markPageLeavingForAnotherDocument);
  window.addEventListener("pageshow", markPageStaying);
  // Captured on the window, so a handler that stops a press from propagating cannot hide it.
  window.addEventListener("pointerdown", markPageStaying, { capture: true });
  window.addEventListener("keydown", markPageStaying, { capture: true });
  document.addEventListener("visibilitychange", markPageStayingWhenVisible);
  window.addEventListener("vite:preloadError", reloadForChunkFailure);

  return () => {
    window.removeEventListener("beforeunload", markPageLeaving);
    navigation?.removeEventListener("navigate", markPageLeavingForAnotherDocument);
    window.removeEventListener("pageshow", markPageStaying);
    window.removeEventListener("pointerdown", markPageStaying, { capture: true });
    window.removeEventListener("keydown", markPageStaying, { capture: true });
    document.removeEventListener("visibilitychange", markPageStayingWhenVisible);
    window.removeEventListener("vite:preloadError", reloadForChunkFailure);
    markPageStaying();
  };
}

// The other half of the offline-chunk policy above: the editor, Yjs, and Hocuspocus are
// code-split so the issue route does not pay for them until a document mounts. A chunk that
// fails to download while the browser is offline is retried once the network returns; Chromium
// caches a failed module fetch in its module map, so that retry can reject again, in which case
// the failure propagates like an online one and reaches `reloadForChunkFailure`, which reloads
// while the tab's budget lasts.
export async function importWhenOnline<T>(load: () => Promise<T>): Promise<T> {
  for (;;) {
    try {
      return await load();
    } catch (error) {
      if (navigator.onLine) {
        throw error;
      }
      await new Promise<void>((resolve) => {
        window.addEventListener("online", () => resolve(), { once: true });
      });
    }
  }
}

/**
 * Keeps long-lived Dispatch tabs able to render Markdown after a SPA deployment, and offers a
 * deliberate reload when the page's entry chunk has been replaced.
 */
export function DeploymentResilience(): ReactNode {
  const [updateAvailable, setUpdateAvailable] = useState(false);
  const dismissed = useRef(false);
  const lastVersionCheckAt = useRef(0);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      void markdownEngine.warmMarkdownRenderer().catch(() => undefined);
    }, 0);
    return () => window.clearTimeout(timer);
  }, []);

  useEffect(() => {
    const checkForUpdate = () => {
      const now = Date.now();
      if (now - lastVersionCheckAt.current < VERSION_CHECK_INTERVAL_MS) {
        return;
      }
      lastVersionCheckAt.current = now;
      void deploymentChanged()
        .then((changed) => {
          if (changed && !dismissed.current) {
            setUpdateAvailable(true);
          }
        })
        .catch(() => undefined);
    };

    window.addEventListener("focus", checkForUpdate);
    return () => window.removeEventListener("focus", checkForUpdate);
  }, []);

  if (!updateAvailable) {
    return null;
  }

  return (
    <section
      aria-label="A new version is available"
      className={`fixed right-4 bottom-4 left-4 z-50 mx-auto flex max-w-md items-center justify-between gap-3 rounded-xl border p-3 shadow-lg ${card} ${borderStrong}`}
      role="status"
    >
      <p className={`text-sm ${textSecondaryOnSurface}`}>A new version is available - Reload</p>
      <div className="flex shrink-0 items-center gap-2">
        <button
          className={`rounded-lg px-3 py-1.5 text-sm font-semibold ${primaryButtonBg} ${primaryButtonHoverBg}`}
          onClick={() => window.location.reload()}
          type="button"
        >
          Reload
        </button>
        <button
          aria-label="Dismiss update notice"
          className={`rounded-lg p-1.5 text-sm ${dismissButtonText} focus-visible:ring-2 ${focusVisibleRing}`}
          onClick={() => {
            dismissed.current = true;
            setUpdateAvailable(false);
          }}
          type="button"
        >
          Dismiss
        </button>
      </div>
    </section>
  );
}
