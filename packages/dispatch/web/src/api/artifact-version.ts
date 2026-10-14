import type { ArtifactVersionContent, ArtifactVersionText } from "./types";

/**
 * Whether a fetched version (`api.getArtifactVersion`) is a document's text, the variant that
 * carries `markdown`, rather than a blob's bytes and checksum.
 */
export function isText(
  content: ArtifactVersionContent | undefined
): content is ArtifactVersionText {
  return content !== undefined && "markdown" in content;
}
