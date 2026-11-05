import { expect, test } from "bun:test";
import { parseIdentityReference } from "./dispatch-identity-ref";

test("identity addresses canonicalize people without changing session or role ids", () => {
  expect(parseIdentityReference("dispatch://people/%20ALICE%40Example.com%20")).toEqual({
    kind: "person",
    id: "alice@example.com",
    reference: "dispatch://people/alice%40example.com",
    href: "/people/alice%40example.com",
  });
  expect(parseIdentityReference("dispatch://sessions/pi-worker-1")?.id).toBe("pi-worker-1");
  expect(parseIdentityReference("dispatch://roles/reviewer")?.id).toBe("reviewer");
  for (const value of [
    "dispatch://people/%00",
    "dispatch://sessions/a%2Fb",
    "dispatch://roles/a/b",
    "dispatch://people/alice#b-fake",
    "dispatch://sessions/%2A",
    "dispatch://roles/review%2A",
    "dispatch://sessions/pi.worker-1",
  ])
    expect(parseIdentityReference(value)).toBeUndefined();
});
