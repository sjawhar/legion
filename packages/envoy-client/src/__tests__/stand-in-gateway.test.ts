import { describe, expect, test } from "bun:test";
import { parseStandInArguments, standInAnswer } from "../stand-in-gateway";

describe("parseStandInArguments", () => {
  // The README promises nginx's 502 page on an ephemeral port when no flag is given.
  test("reads each flag in any order, defaulting to a 502 HTML page on an ephemeral port", () => {
    expect(parseStandInArguments([])).toEqual({ status: 502, body: "html", port: 0 });
    expect(parseStandInArguments(["--port", "18657", "--body", "json", "--status", "504"])).toEqual(
      { status: 504, body: "json", port: 18657 }
    );
  });

  // A typo must stop the stand-in rather than serve an answer nobody asked for: `Number("5o3")` is
  // NaN, and `new Response` throws on any status outside 200-599 only once a request arrives.
  test("refuses a flag without a value, an unknown flag or body, and a status or port out of range", () => {
    expect(() => parseStandInArguments(["--status"])).toThrow("--status needs a value");
    expect(() => parseStandInArguments(["--host", "0.0.0.0"])).toThrow(
      "unknown option --host 0.0.0.0"
    );
    expect(() => parseStandInArguments(["--body", "xml"])).toThrow(
      "--body must be html, empty, text or json, not xml"
    );
    for (const status of ["5o3", "199", "600", "502.5"]) {
      expect(() => parseStandInArguments(["--status", status])).toThrow(
        `--status must be an integer from 200 to 599, not ${status}`
      );
    }
    for (const port of ["-1", "65536", "http"]) {
      expect(() => parseStandInArguments(["--port", port])).toThrow(
        `--port must be an integer from 0 to 65535, not ${port}`
      );
    }
  });
});

describe("standInAnswer", () => {
  // Each kind is one way a real gateway answers in Dispatch's place; the client must tell every
  // one of them from Dispatch's own `{code, error}` JSON.
  test("answers as nginx, an empty body, Envoy proxy's line or JSON of another shape, under the status", async () => {
    const html = standInAnswer(404, "html");
    expect(html.status).toBe(404);
    expect(html.headers.get("content-type")).toBe("text/html");
    expect(await html.text()).toBe(
      "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found" +
        "</h1></center>\r\n<hr><center>nginx/1.25.3</center>\r\n</body>\r\n</html>\r\n"
    );
    expect(await standInAnswer(522, "html").text()).toContain("<title>522 Gateway</title>");

    const empty = standInAnswer(503, "empty");
    expect(empty.status).toBe(503);
    expect(empty.headers.get("content-type")).toBeNull();
    expect(await empty.text()).toBe("");

    const text = standInAnswer(503, "text");
    expect(text.headers.get("content-type")).toBe("text/plain");
    expect(await text.text()).toBe("upstream connect error or disconnect/reset before headers");

    const json = standInAnswer(504, "json");
    expect(json.status).toBe(504);
    expect(json.headers.get("content-type")).toBe("application/json");
    expect(await json.json()).toEqual({ message: "upstream request timeout" });
  });
});
