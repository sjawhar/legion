import { describe, expect, test } from "bun:test";

import { DISPATCH_HREF_REFERENCES, itemFromSearch } from "./dispatch-href";

describe("itemFromSearch", () => {
  test("names the item a document URL carries", () => {
    expect(itemFromSearch("?comment=c1")).toEqual({ id: "c1", kind: "comment" });
    expect(itemFromSearch("?ask=a1")).toEqual({ id: "a1", kind: "ask" });
    expect(itemFromSearch("?ask=a%201")).toEqual({ id: "a 1", kind: "ask" });
  });

  test("names none when the query carries none", () => {
    expect(itemFromSearch("")).toBeUndefined();
    expect(itemFromSearch("?v=3")).toBeUndefined();
  });

  test("refuses a query Dispatch never emits rather than guessing", () => {
    expect(itemFromSearch("?ask=a1&comment=c1")).toBeNull();
    expect(itemFromSearch("?comment=")).toBeNull();
    expect(itemFromSearch("?comment=a/b")).toBeNull();
    expect(itemFromSearch("?comment=%E0%A4%A")).toBeNull();
  });
});

test("the golden table covers both item shapes on every document path", () => {
  const items = DISPATCH_HREF_REFERENCES.filter((row) => row.href.includes("?"));
  expect(items.map((row) => row.href)).toEqual([
    "/issues/CORE-1/spec?comment=44444444-4444-4444-8444-444444444444",
    "/issues/CORE-1/artifacts/notes-md?comment=55555555-5555-4555-8555-555555555555",
    "/issues/CORE-1/spec?ask=66666666-6666-4666-8666-666666666666",
    "/issues/CORE-1/artifacts/notes-md?ask=77777777-7777-4777-8777-777777777777",
    "/projects/CORE/documents/handbook-md?comment=88888888-8888-4888-8888-888888888888",
    "/projects/CORE/documents/handbook-md?ask=99999999-9999-4999-8999-999999999999",
  ]);
});

test("the Go reader's table is the same table", async () => {
  // `packages/envoy/internal/dispatch/text` cannot import TypeScript, so it walks a JSON copy.
  // Regenerate it from here rather than editing it:
  //   bun -e 'import {DISPATCH_HREF_REFERENCES as t} from "@legion/contracts";
  //           console.log(JSON.stringify(t, null, 2))' > <that file>
  const path = new URL(
    "../../envoy/internal/dispatch/text/testdata/dispatch-href-references.json",
    import.meta.url
  );
  expect(JSON.parse(await Bun.file(path).text())).toEqual([...DISPATCH_HREF_REFERENCES]);
});
