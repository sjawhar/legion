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
import * as MarkdownBody from "../refs/MarkdownBody";

const CHUNK_RELOAD_STORAGE_KEY = "dispatch.reloaded-for-chunk";
const VERSION_CHECK_INTERVAL_MS = 60_000;
const INDEX_ASSET_PATH = /^\/assets\/index-[^/]+\.js$/;

declare const __DISPATCH_BUILD__: string;

// The Navigation API, as much of it as this module uses: TypeScript's DOM library does not declare
// it yet, and a browser without it leaves `window.navigation` undefined.
interface NavigateEvent extends Event {
  readonly destination: { readonly sameDocument: boolean };
}

interface Navigation {
  addEventListener(type: "navigate", listener: (event: NavigateEvent) => void): void;
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
// prompt, stopped, or answered with a download - and no event says so; the press or the return
// that follows is what ends the state, never a timer, since a refusal can arrive any number of
// tasks after the navigation started.
let leavingPage = false;

function markPageLeaving(): void {
  leavingPage = true;
}

// A route change inside the app is a navigation within this document; only one to another document
// leaves the page. iOS Safari fires `navigate` from 26.2, for a link followed or a form submitted
// but not for an address the reader types.
function markPageLeavingForAnotherDocument(event: NavigateEvent): void {
  if (!event.destination.sameDocument) {
    leavingPage = true;
  }
}

function markPageStaying(): void {
  leavingPage = false;
}

function markPageStayingWhenVisible(): void {
  if (document.visibilityState === "visible") {
    leavingPage = false;
  }
}

// Reloads the page once per session for a chunk that failed to download while online.
function reloadForChunkFailure(): void {
  if (leavingPage) {
    return;
  }
  // A chunk that fails to download while the browser is offline is a network outage, not a
  // replaced deployment: reloading now would swap the app for the browser's offline page.
  if (!navigator.onLine) {
    return;
  }
  if (window.sessionStorage.getItem(CHUNK_RELOAD_STORAGE_KEY) === "true") {
    return;
  }
  window.sessionStorage.setItem(CHUNK_RELOAD_STORAGE_KEY, "true");
  window.location.reload();
}

// Vite dispatches `vite:preloadError` for a chunk or one of its stylesheets that failed to
// download, and throws the failure to the importer unless the event is default-prevented. A
// prevented failure of the chunk itself resolves the import with `undefined`, which the importer
// reads as a module and fails on with a TypeError that says nothing about the download; a
// prevented stylesheet failure loads the module without its styles. So the handler never
// prevents it: the importer always sees the failure itself, and the page also reloads at most
// once per session, never while it is being left.
export function installChunkFailureRecovery(): void {
  window.addEventListener("beforeunload", markPageLeaving);
  window.navigation?.addEventListener("navigate", markPageLeavingForAnotherDocument);
  window.addEventListener("pageshow", markPageStaying);
  // Captured on the window, so a handler that stops a press from propagating cannot hide it.
  window.addEventListener("pointerdown", markPageStaying, { capture: true });
  window.addEventListener("keydown", markPageStaying, { capture: true });
  document.addEventListener("visibilitychange", markPageStayingWhenVisible);
  window.addEventListener("vite:preloadError", reloadForChunkFailure);
}

// The other half of the offline-chunk policy above: the editor, Yjs, and Hocuspocus are
// code-split so the issue route does not pay for them until a document mounts. A chunk that
// fails to download while the browser is offline is retried once the network returns; Chromium
// caches a failed module fetch in its module map, so that retry can reject again, in which case
// the failure propagates like an online one and `reloadForChunkFailure` has already reloaded
// once per session.
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
      void MarkdownBody.warmMarkdownRenderer().catch(() => undefined);
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
