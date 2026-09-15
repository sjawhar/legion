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

function reloadForChunkFailure(event: Event): void {
  // A chunk that fails to download while the browser is offline is a network outage, not a
  // replaced deployment: reloading now would swap the app for the browser's offline page. The
  // error propagates to the importer, whose lazy boundary retries once the network returns.
  if (!navigator.onLine) {
    return;
  }
  event.preventDefault();
  if (window.sessionStorage.getItem(CHUNK_RELOAD_STORAGE_KEY) === "true") {
    return;
  }
  window.sessionStorage.setItem(CHUNK_RELOAD_STORAGE_KEY, "true");
  window.location.reload();
}

export function installChunkFailureRecovery(): void {
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
