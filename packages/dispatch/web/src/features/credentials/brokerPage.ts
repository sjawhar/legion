import { safeExternalHref } from "../../lib/external-href";

/** A secret request's page on the secrets broker, where a person signed in to the broker decides
 *  it. The broker names it as `approve_url` on every answer that names a pending secret request,
 *  and Dispatch relays that answer's body verbatim, so the value is the broker's word and not
 *  Dispatch's: `safeExternalHref` takes it only as an absolute `http:` or `https:` URL, and
 *  anything else, a `javascript:` URL or a relative path among them, reads as absent, since a
 *  link or a redirect would otherwise run or open whatever the relayed body named. A broker older
 *  than that page names none: the Inbox row then links to the record's own Dispatch page, and that
 *  page decides the request itself, which is what both did before the broker had a page at all.
 */
export function brokerPage(approveUrl: string | undefined): string | null {
  return approveUrl === undefined ? null : safeExternalHref(approveUrl);
}
