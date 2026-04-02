import { afterEach, describe, expect, test } from "bun:test";
import { readGauges, registerGauges, stop, takeSnapshot } from "../telemetry";

afterEach(() => {
  stop();
});

describe("daemon telemetry", () => {
  test("merges registered gauges into snapshots", () => {
    const releaseA = registerGauges("telemetry-test-a", () => ({
      test_workers: 3,
    }));
    const releaseB = registerGauges("telemetry-test-b", () => ({
      test_crashes: 1,
    }));

    const gauges = readGauges();
    expect(gauges.test_workers).toBe(3);
    expect(gauges.test_crashes).toBe(1);

    const snap = takeSnapshot();
    expect(snap.gauges.test_workers).toBe(3);
    expect(snap.gauges.test_crashes).toBe(1);

    releaseA();
    releaseB();
  });
});
