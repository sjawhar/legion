import { describe, expect, it } from "bun:test";
import { createCancellableSleep } from "../cancellable-sleep";

describe("createCancellableSleep", () => {
  it("resolves an in-flight sleep immediately on cancel, without waiting out the delay", async () => {
    const cancellableSleep = createCancellableSleep();
    let settled = false;
    const promise = cancellableSleep.sleep(60_000).then(() => {
      settled = true;
    });

    expect(settled).toBe(false);
    cancellableSleep.cancel();
    await promise;

    expect(settled).toBe(true);
  });

  it("is a no-op when cancelled with no sleep pending", () => {
    const cancellableSleep = createCancellableSleep();

    expect(() => cancellableSleep.cancel()).not.toThrow();
  });
});
