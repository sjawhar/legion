import { expect } from "bun:test";

/** The bounds one attempt of a write the page makes carries, as a spied client method receives
 *  them: the key of the press it belongs to (`Idempotency-Key`), which every retry of that press
 *  reuses. A test that asserts a write's arguments names this as the last one. */
export const keyedAttempt = expect.objectContaining({ key: expect.any(String) });
