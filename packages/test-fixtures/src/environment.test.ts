import { afterAll, expect, test } from "bun:test";
import { captureEnvironment, restoreEnvironment } from "./environment";

/** A name this file gives a value before it captures it. */
const SET = "TEST_FIXTURES_ENVIRONMENT_SET";
/** A name this file leaves unset, so a restore of it must delete it again. */
const UNSET = "TEST_FIXTURES_ENVIRONMENT_UNSET";

process.env[SET] = "the caller's value";
delete process.env[UNSET];

restoreEnvironment([SET, UNSET]);

afterAll(() => {
  delete process.env[SET];
  delete process.env[UNSET];
});

test("a test may set both names", () => {
  process.env[SET] = "changed by this test";
  process.env[UNSET] = "added by this test";
  expect(process.env[SET]).toBe("changed by this test");
  expect(process.env[UNSET]).toBe("added by this test");
});

test("the next test finds the set name back and the added name deleted", () => {
  expect(process.env[SET]).toBe("the caller's value");
  expect(process.env[UNSET]).toBeUndefined();
});

test.failing("a test that throws after it changed both names", () => {
  process.env[SET] = "changed by a test that throws";
  process.env[UNSET] = "added by a test that throws";
  throw new Error("this test throws on purpose: the restore must run after it");
});

test("the test after the throwing one finds both put back", () => {
  expect(process.env[SET]).toBe("the caller's value");
  expect(process.env[UNSET]).toBeUndefined();
});

test("captureEnvironment's restore puts back a set name and deletes an unset one", () => {
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
