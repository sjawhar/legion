import { isSessionId, referenceTargetFits } from "./dispatch-text-rules";

export interface IdentityReference {
  readonly kind: "person" | "session" | "role";
  readonly id: string;
  readonly reference: string;
  readonly href: string;
}

/** Identity addresses share the dispatch:// grammar, not the direct-message address space. */
export function parseIdentityReference(value: string): IdentityReference | undefined {
  const match = /^dispatch:\/\/(people|sessions|roles)\/([^/?#]+)$/.exec(value);
  if (match === null) return undefined;
  let id: string;
  try {
    id = decodeURIComponent(match[2] ?? "");
  } catch {
    return undefined;
  }
  const namespace = match[1];
  if (namespace === "people") id = id.trim().toLowerCase();
  if (!isSessionId(id) || !referenceTargetFits(id)) return undefined;
  const kind = namespace === "people" ? "person" : namespace === "sessions" ? "session" : "role";
  const reference = `dispatch://${namespace}/${encodeURIComponent(id)}`;
  const href =
    kind === "person"
      ? `/people/${encodeURIComponent(id)}`
      : kind === "session"
        ? `/agents/${encodeURIComponent(id)}/live`
        : `/agents?role=${encodeURIComponent(id)}`;
  return { kind, id, reference, href };
}
