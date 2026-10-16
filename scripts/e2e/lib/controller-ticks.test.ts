import { describe, expect, test } from "bun:test";
import { runJq } from "./run-jq";

// lib/controller-ticks.jq, as stage4b-sandbox-tree.sh's controller checkpoint loads it: the
// openCapabilities list read from a tick delivery pi-envoy rendered as TOON, the ticks of a
// controller session read from its transcript, and the verdict that every tick names the open rows
// of the daemon running now. The finding the verdict holds: three `legion start`s run between boot
// and the controller checkpoint, each re-probes the image, and pool-capacity is each probe's own
// verdict, so a run that scaled the pool at boot and found it warm at the restarts carries
// ["secrets"] on its ticks against boot's two rows.
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

describe("ticks_name_open($open): every tick names the running daemon's open rows", () => {
  const verdict = (ticks: string[][], open: string[]) =>
    jq(`ticks_name_open(${JSON.stringify(open)})`, JSON.stringify(ticks.map((open) => ({ open }))));

  test("a warm pool: ticks and the daemon both name secrets alone", () => {
    expect(verdict([["secrets"], ["secrets"]], ["secrets"])).toBe(true);
  });

  test("a cold pool healed at a later start: the ticks are the running daemon's rows, not boot's", () => {
    expect(verdict([["secrets"]], ["secrets"])).toBe(true);
    expect(verdict([["secrets"]], ["secrets", "pool-capacity"])).toBe(false);
  });

  test("a pool still cold: both rows, in either order", () => {
    expect(verdict([["pool-capacity", "secrets"]], ["secrets", "pool-capacity"])).toBe(true);
  });

  test("one divergent tick among agreeing ones", () => {
    expect(verdict([["secrets"], ["secrets", "pool-capacity"], ["secrets"]], ["secrets"])).toBe(
      false
    );
  });

  test("open rows that no longer name secrets", () => {
    expect(verdict([["pool-capacity"]], ["pool-capacity"])).toBe(false);
  });

  test("no ticks", () => {
    expect(verdict([], ["secrets"])).toBe(false);
  });
});
