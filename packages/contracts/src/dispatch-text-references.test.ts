import { expect, test } from "bun:test";

import { DISPATCH_TEXT_REFERENCES } from "./dispatch-text-references";

test("the Go reader's text table is the same table", async () => {
  // `packages/envoy/internal/dispatch/text` cannot import TypeScript, so it walks a JSON copy.
  // Regenerate it from here rather than editing it:
  //   bun -e 'import {DISPATCH_TEXT_REFERENCES as t} from "@legion/contracts";
  //           console.log(JSON.stringify(t, null, 2))' > <that file>
  const path = new URL(
    "../../envoy/internal/dispatch/text/testdata/dispatch-text-references.json",
    import.meta.url
  );
  expect(JSON.parse(await Bun.file(path).text())).toEqual([...DISPATCH_TEXT_REFERENCES]);
});
