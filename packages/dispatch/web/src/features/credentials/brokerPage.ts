/** A secret request's page on the secrets broker, where a person signed in to the broker decides
 *  it. The broker names it as `approve_url` on every answer that names a pending secret request,
 *  and Dispatch relays that answer's body verbatim, so the value is the broker's word and not
 *  Dispatch's: it is used only when it is an absolute `http:` or `https:` URL. Anything else, a
 *  `javascript:` URL or a relative path among them, reads as absent, since a link or a redirect
 *  would otherwise run or open whatever the relayed body named. A broker older than that page
 *  names none, and every caller then keeps what it did before.
 */
export function brokerPage(approveUrl: string | undefined): string | null {
  if (approveUrl === undefined) {
    return null;
  }
  try {
    const url = new URL(approveUrl);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : null;
  } catch {
    return null;
  }
}
