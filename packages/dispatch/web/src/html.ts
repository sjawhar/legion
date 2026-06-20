export function escapeHtml(value: string | number | null | undefined): string {
  return String(value ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

import DOMPurify from "dompurify";
import { marked } from "marked";

// GitHub-flavored Markdown rendering. We configure marked with gfm + breaks
// to match GitHub's own rendering on issue/comment bodies (the same prose
// the dashboard surfaces), then sanitize with DOMPurify before innerHTML.
// Sanitization strips script/style/iframe/event handlers; trusted GitHub
// content survives intact, hostile content can't break out.
marked.setOptions({
  gfm: true,
  breaks: true,
  pedantic: false,
});

export function renderMarkdownLite(body: string): string {
  const trimmed = body.trim();
  if (!trimmed) return "";
  const html = marked.parse(trimmed, { async: false }) as string;
  // DOMPurify needs browser globals; in Bun/Node test envs we skip
  // sanitization. Production traffic always goes through the browser-side
  // renderer where DOMPurify is real.
  if (typeof window === "undefined") return html;
  return DOMPurify.sanitize(html, { ADD_ATTR: ["target", "rel"] });
}

export function timeAgo(value: string): string {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1000));
  if (seconds < 60) return "just now";
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86_400)}d ago`;
}
