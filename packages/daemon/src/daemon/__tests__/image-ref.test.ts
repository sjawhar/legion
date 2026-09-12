import { describe, expect, it } from "bun:test";
import { IMAGE_DIGEST_REQUIRED, parseImageDigestRef } from "../image-ref";

const DIGEST = `sha256:${"a".repeat(64)}` as const;

describe("parseImageDigestRef", () => {
  it("accepts a digest-pinned reference and splits name from digest", () => {
    expect(parseImageDigestRef(`ghcr.io/sjawhar/legion-worker@${DIGEST}`)).toEqual({
      reference: `ghcr.io/sjawhar/legion-worker@${DIGEST}`,
      name: "ghcr.io/sjawhar/legion-worker",
      digest: DIGEST,
    });
  });

  it("keeps an informational tag beside the digest (the digest still pins)", () => {
    expect(parseImageDigestRef(`ghcr.io/sjawhar/legion-worker:1.2.3@${DIGEST}`).name).toBe(
      "ghcr.io/sjawhar/legion-worker:1.2.3"
    );
  });

  it("refuses a tag-only reference with the configured-field message", () => {
    expect(() => parseImageDigestRef("ghcr.io/sjawhar/legion-worker:1.2.3")).toThrow(
      IMAGE_DIGEST_REQUIRED
    );
    expect(IMAGE_DIGEST_REQUIRED).toBe(
      "runtime.kubernetes.image must be pinned by digest (@sha256:…)"
    );
  });

  it("refuses malformed digests: short, wrong algorithm, uppercase hex, empty", () => {
    for (const bad of [
      "ghcr.io/sjawhar/legion-worker@sha256:abc",
      `ghcr.io/sjawhar/legion-worker@md5:${"a".repeat(32)}`,
      `ghcr.io/sjawhar/legion-worker@sha256:${"A".repeat(64)}`,
      "",
    ]) {
      expect(() => parseImageDigestRef(bad)).toThrow(IMAGE_DIGEST_REQUIRED);
    }
  });
});
