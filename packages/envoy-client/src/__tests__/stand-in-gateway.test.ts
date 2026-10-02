import { describe, expect, test } from "bun:test";
import { DispatchClient, DispatchGatewayError } from "../dispatch-http";
import { parseStandInArguments, standInAnswer } from "../stand-in-gateway";

describe("parseStandInArguments", () => {
  // The README promises nginx's 502 page on an ephemeral port when no flag is given.
  test("reads each flag in either form and any order, defaulting to a 502 HTML page on an ephemeral port", () => {
    expect(parseStandInArguments([])).toEqual({ status: 502, body: "html", port: 0 });
    expect(parseStandInArguments(["--port", "18657", "--body=json", "--status", "504"])).toEqual({
      status: 504,
      body: "json",
      port: 18657,
    });
  });

  // A typo must stop the stand-in rather than serve an answer nobody asked for: `Number("5o3")` is
  // NaN, and `new Response` throws on any status outside 200-599 only once a request arrives.
  test("refuses an unknown body, and a status or port out of range", () => {
    expect(() => parseStandInArguments(["--body", "xml"])).toThrow(
      "--body must be html, empty, text or json, not xml"
    );
    for (const status of ["5o3", "199", "600", "502.5"]) {
      expect(() => parseStandInArguments(["--status", status])).toThrow(
        `--status must be an integer from 200 to 599, not ${status}`
      );
    }
    for (const port of ["-1", "65536", "http"]) {
      expect(() => parseStandInArguments([`--port=${port}`])).toThrow(
        `--port must be an integer from 0 to 65535, not ${port}`
      );
    }
  });
});

describe("standInAnswer", () => {
  // The stand-in exists to show what a tool says when something other than Dispatch answers, so
  // no kind may ever read as Dispatch's own `{code, error}`: each must reach the client as a
  // gateway's answer under the configured status.
  test("gives every kind of answer under the status, and the client reads each as a gateway's", async () => {
    for (const body of ["html", "empty", "text", "json"] as const) {
      const client = new DispatchClient("http://dispatch.test", "secret", (async () =>
        standInAnswer(504, body)) as unknown as typeof fetch);

      const error = await client.getIssue("DSP-1").then(
        () => undefined,
        (failure: unknown) => failure
      );

      expect(error).toBeInstanceOf(DispatchGatewayError);
      expect(error).toEqual(expect.objectContaining({ status: 504, code: "HTTP_504" }));
    }
  });
});
