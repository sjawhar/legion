export interface ImageDigestRef {
  /** The reference exactly as configured, e.g. `ghcr.io/sjawhar/legion-worker@sha256:…`. */
  readonly reference: string;
  /** Everything before `@`: registry/repository, with an optional informational tag. */
  readonly name: string;
  readonly digest: `sha256:${string}`;
}

export const IMAGE_DIGEST_REQUIRED =
  "runtime.kubernetes.image must be pinned by digest (@sha256:…)";
const DIGEST_REF = /^([^@\s]+)@(sha256:[0-9a-f]{64})$/;

/** Accepts only `<name>[:tag]@sha256:<64 lowercase hex>`. A tag is mutable and the daemon must know
 * exactly what it probed, so a tag-only or malformed reference is refused with `IMAGE_DIGEST_REQUIRED`.
 * Consumed by LEGION-24 when it wires `runtime.kubernetes.image` into config.ts. */
export function parseImageDigestRef(reference: string): ImageDigestRef {
  const match = DIGEST_REF.exec(reference);
  if (!match) throw new Error(IMAGE_DIGEST_REQUIRED);
  return { reference, name: match[1] as string, digest: match[2] as `sha256:${string}` };
}
