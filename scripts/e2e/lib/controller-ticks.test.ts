import { describe, expect, test } from "bun:test";
import { runJq } from "./run-jq";

// lib/controller-ticks.jq, as stage4b-sandbox-tree.sh's controller checkpoint loads it: the
// openCapabilities list read from a tick delivery pi-envoy rendered as TOON, the ticks of a
// controller session read from its transcript, and the verdict that judges each tick alone against
// the two lists the deployment can open. The finding behind the per-tick verdict: three `legion
// start`s run between boot and the controller checkpoint, each re-probes the image, and
// pool-capacity is each probe's own verdict, so one session's ticks can carry
// ["secrets", "pool-capacity"] and ["secrets"] both, and a snapshot of the running daemon's open rows
// would hold only while nothing restarts it or opens a live row before the read.
const lib = import.meta.dir;
// jq runs PROGRAM with the module on the include path over STDIN: JSON, or, when RAW, one string
// read as the script reads a session or a delivery (`jq -R -s`).
const jq = (program: string, stdin: string, raw = false): unknown =>
  JSON.parse(
    runJq(
      [...(raw ? ["-R", "-s"] : []), "-c", "-L", lib, `include "controller-ticks"; ${program}`],
      stdin
    )
  );

const needle = "tick on LEGSMOKE";
// rendered is a tick delivery's card as pi-envoy renders it, with LIST as its openCapabilities
// line, or none.
const rendered = (list: string | null) =>
  `Envoy message\n  summary: ${needle}\n  message:\n    kind: tick\n${
    list === null ? "" : `    openCapabilities[${list.split(",").length}]: ${list}\n`
  }  from: legion-daemon`;

describe("tick_open_capabilities", () => {
  test("a list of one", () => {
    expect(jq("tick_open_capabilities", rendered("secrets"), true)).toEqual(["secrets"]);
  });

  test("a list of two", () => {
    expect(jq("tick_open_capabilities", rendered("secrets,pool-capacity"), true)).toEqual([
      "secrets",
      "pool-capacity",
    ]);
  });

  test("a delivery without the line names no gap", () => {
    expect(jq("tick_open_capabilities", rendered(null), true)).toEqual([]);
  });
});

describe("controller_ticks($tick)", () => {
  const envoy = (content: unknown) =>
    JSON.stringify({ type: "custom_message", customType: "envoy-message", content });
  // rest is the session after the tick: an envoy-message that is no tick, and a line that is no
  // custom message though it carries the needle.
  const rest = `${envoy("Envoy message\n  summary: held on LEGSMOKE-3\n  message:\n    kind: held")}\n${JSON.stringify(
    { type: "message", message: { role: "assistant", content: [{ type: "text", text: needle }] } }
  )}\n`;
  const program = `controller_ticks(${JSON.stringify(needle)})`;

  test("the envoy-message deliveries carrying the needle, each with its list and trimmed text", () => {
    expect(jq(program, `${envoy(rendered("secrets"))}\n${rest}`, true)).toEqual([
      {
        open: ["secrets"],
        delivery:
          "Envoy message summary: tick on LEGSMOKE message: kind: tick openCapabilities[1]: secrets from: legion-daemon",
      },
    ]);
  });

  test("content as text blocks is read the same", () => {
    const blocks = [
      { type: "text", text: rendered("secrets,pool-capacity") },
      { type: "image", data: "iVBORw0KGgo=", mimeType: "image/png" },
    ];
    expect(jq(`${program} | map(.open)`, `${envoy(blocks)}\n${rest}`, true)).toEqual([
      ["secrets", "pool-capacity"],
    ]);
  });
});

describe("ticks_name_deployment_open: each tick names one of the two lists the deployment can open", () => {
  const verdict = (ticks: string[][]) =>
    jq("ticks_name_deployment_open", JSON.stringify(ticks.map((open) => ({ open }))));

  test("a warm pool: secrets alone", () => {
    expect(verdict([["secrets"]])).toBe(true);
  });

  test("a cold pool: secrets and pool-capacity, in either order", () => {
    expect(verdict([["secrets", "pool-capacity"]])).toBe(true);
    expect(verdict([["pool-capacity", "secrets"]])).toBe(true);
  });

  test("a pool cold at one start and warm at the next: both arms in one session", () => {
    expect(verdict([["secrets", "pool-capacity"], ["secrets"]])).toBe(true);
  });

  test("a tick that does not name secrets", () => {
    expect(verdict([["pool-capacity"]])).toBe(false);
    expect(verdict([[]])).toBe(false);
  });

  test("a tick naming a row the deployment cannot open", () => {
    expect(verdict([["secrets", "model-fallback"]])).toBe(false);
  });

  test("a tick naming a live row", () => {
    expect(verdict([["secrets", "web-search"]])).toBe(false);
  });

  test("one bad tick fails the session", () => {
    expect(verdict([["secrets"], ["secrets", "model-fallback"]])).toBe(false);
  });

  test("no ticks", () => {
    expect(verdict([])).toBe(false);
  });
});
