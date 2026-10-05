import { expect, test } from "bun:test";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { withDeliveredPictures } from "./delivery-pictures";

const session = "01a1058e-f14f-7684-87eb-3dc885955551";
const address = `dispatch://agent/${session}/artifact/shot-png@v1`;
/** A picture Dispatch no longer serves. */
const gone = `dispatch://agent/${session}/artifact/gone-png@v1`;
/** A picture whose read never answers: the delivery's timeout, or Dispatch unreachable. */
const unreachable = `dispatch://agent/${session}/artifact/slow-png@v1`;
const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2]);

function client(): DispatchClient {
  const fetchImpl = async (url: string | URL | Request): Promise<Response> => {
    const { pathname } = new URL(String(url));
    if (pathname === `/api/v1/agents/${session}/artifacts/shot-png`) {
      return Response.json({
        id: "pic-1",
        name: "shot.png",
        kind: "image",
        versions: [{ number: 1, mime: "image/png", size: png.length }],
      });
    }
    if (pathname === "/api/v1/artifacts/pic-1/versions/1") {
      return new Response(png, { headers: { "Content-Type": "image/png" } });
    }
    if (pathname === `/api/v1/agents/${session}/artifacts/gone-png`) {
      return Response.json({ code: "NOT_FOUND", error: "artifact not found" }, { status: 404 });
    }
    if (pathname === `/api/v1/agents/${session}/artifacts/slow-png`) {
      throw new DOMException("The operation timed out.", "TimeoutError");
    }
    throw new Error(`unexpected request: ${pathname}`);
  };
  return new DispatchClient("http://dispatch.test", "secret", fetchImpl as typeof fetch);
}

const image = {
  type: "image" as const,
  data: Buffer.from(png).toString("base64"),
  mimeType: "image/png",
};

test("a delivered card carries its pictures as image blocks after the text that names them", async () => {
  expect(await withDeliveredPictures("envoy: card", [address, address], client, true)).toEqual([
    {
      type: "text",
      text: `envoy: card\n\nPictures:\n- image 1: ${address} (shot.png, image/png, 10 bytes)`,
    },
    image,
  ]);
});

test("a person's own turn keeps their text exactly, with their pictures beside it", async () => {
  const body = `Look:\n\n![shot.png](${address})`;
  expect(await withDeliveredPictures(body, [address], client, false)).toEqual([
    { type: "text", text: body },
    image,
  ]);
});

test("a picture Dispatch cannot serve is named and costs the delivery neither its text nor its other pictures", async () => {
  const content = await withDeliveredPictures(
    "envoy: card",
    [address, gone, unreachable],
    client,
    true
  );

  const [card, ...pictures] = Array.isArray(content) ? content : [];
  expect(pictures).toEqual([image]);
  const lines = card?.type === "text" ? card.text.split("\n") : [];
  expect(lines.slice(0, 4)).toEqual([
    "envoy: card",
    "",
    "Pictures:",
    `- image 1: ${address} (shot.png, image/png, 10 bytes)`,
  ]);
  expect(lines.slice(4).map((line) => line.split(" (unavailable: ")[0])).toEqual([
    `- not shown: ${gone}`,
    `- not shown: ${unreachable}`,
  ]);
  // Every picture unavailable: the delivery goes out as its text alone.
  expect(await withDeliveredPictures("plain", [gone, unreachable], client, true)).toBe("plain");
});

test("a Dispatch configuration that cannot be read leaves the delivery its text", async () => {
  const broken = (): DispatchClient | undefined => {
    throw new Error("dispatch config: DISPATCH_TOKEN_FILE cannot be read");
  };
  expect(await withDeliveredPictures("envoy: card", [address], broken, true)).toBe("envoy: card");
});

test("a delivery with no pictures, or no Dispatch to read them from, stays text", async () => {
  expect(await withDeliveredPictures("plain", [], client, true)).toBe("plain");
  expect(await withDeliveredPictures("plain", [address], () => undefined, true)).toBe("plain");
});
