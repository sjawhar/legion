/** `value` as an address a link or a redirect may follow: an absolute `http:` or `https:` URL, in
 *  the form `URL` parses it into. Anything else - a `javascript:` URL, a relative path, a string
 *  that is no URL at all - answers null, since following it would run or open whatever the string
 *  named. Every value checked here comes from outside Dispatch: an issue's stored external link
 *  (`features/issue/GitHubLink.tsx`) and the secrets broker's own page for a credential record
 *  (`features/credentials/brokerPage.ts`), which Dispatch relays verbatim. The two read one answer
 *  to "which URL is safe to follow", so neither can drift into trusting what the other refuses. */
export function safeExternalHref(value: string): string | null {
  try {
    const url = new URL(value);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : null;
  } catch {
    return null;
  }
}
