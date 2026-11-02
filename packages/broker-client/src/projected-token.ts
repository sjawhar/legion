// packages/broker-client/src/projected-token.ts

import { readFile } from "node:fs/promises";
import type { SubjectToken, TokenSource } from "./source";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "./wire";

/**
 * The identity of a pod: the projected Kubernetes service-account token its runtime mounts at
 * `path`, minted for the broker's exchange audience. The kubelet rewrites that file in place well
 * before the token expires, so it is read at every trade and never once at start.
 */
export function projectedToken(path: string): TokenSource {
  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      let contents: string;
      try {
        contents = await readFile(path, {
          encoding: "utf8",
          ...(signal === undefined ? {} : { signal }),
        });
      } catch (error) {
        throw new Error(
          `read the projected service-account token ${path}: ${(error as Error).message}`
        );
      }
      const token = contents.trim();
      if (token === "") throw new Error(`the projected service-account token ${path} is empty`);
      return { token, tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES };
    },
    describe: () => `the projected service-account token ${path}`,
  };
}
