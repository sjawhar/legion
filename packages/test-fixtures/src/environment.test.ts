import { expect, test } from "bun:test";
import { captureEnvironment, restoreEnvironment } from "./environment";

/** A name a row of this file gives a value; the module body sets nothing, because a run that
 *  selects no row of this file runs neither the `afterEach` nor the `afterAll` that would put it
 *  back, and the name would stay set for every later file of the same `bun test` process. */
const SET = "TEST_FIXTURES_ENVIRONMENT_SET";
/** A second name, used the same way. */
const UNSET = "TEST_FIXTURES_ENVIRONMENT_UNSET";

// Both names hold no value in the caller's process, so the restore must delete each one again.
restoreEnvironment([SET, UNSET]);

test("a test may set both names", () => {
  process.env[SET] = "changed by this test";
  process.env[UNSET] = "added by this test";
  expect(process.env[SET]).toBe("changed by this test");
  expect(process.env[UNSET]).toBe("added by this test");
});

test("the next test finds both names deleted again", () => {
  expect(process.env[SET]).toBeUndefined();
  expect(process.env[UNSET]).toBeUndefined();
});

test.failing("a test that throws after it changed both names", () => {
  process.env[SET] = "changed by a test that throws";
  process.env[UNSET] = "added by a test that throws";
  throw new Error("this test throws on purpose: the restore must run after it");
});

test("the test after the throwing one finds both put back", () => {
  expect(process.env[SET]).toBeUndefined();
  expect(process.env[UNSET]).toBeUndefined();
});

test("captureEnvironment's restore puts back a set name and deletes an unset one", () => {
  // The set half, which the hooks above cannot show without a write at module scope: the value is
  // given here, captured here, and the `afterEach` the file registered deletes the name again.
  process.env[SET] = "the caller's value";
  const restore = captureEnvironment([SET, UNSET]);
  try {
    process.env[SET] = "changed inside the try";
    process.env[UNSET] = "added inside the try";
    throw new Error("the body failed");
  } catch (error) {
    expect((error as Error).message).toBe("the body failed");
  } finally {
    restore();
  }
  expect(process.env[SET]).toBe("the caller's value");
  expect(process.env[UNSET]).toBeUndefined();
});
