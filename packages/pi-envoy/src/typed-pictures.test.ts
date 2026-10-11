import { describe, expect, test } from "bun:test";
import type { ImageContent } from "@legion/pi-shared/pi-types";
import { TypedPictures } from "./typed-pictures";

const png = (data: string): ImageContent => ({ data, mimeType: "image/png", type: "image" });

/** The host's message a typed input submitted, carrying its Dispatch id as `tag`. */
const tagged = (tag: string) => ({ content: "Look", role: "user", tag, timestamp: 10 });

/**
 * A typed turn's pictures, from the send until the host's message carrying its Dispatch id starts.
 * The host asks pi-envoy's `input` handler for an input's images by its text alone, so a turn is
 * handed its pictures by text in send order; only a turn that was handed them counts them shown.
 */
describe("a typed turn's pictures", () => {
  test("are handed to inputs with their text in send order, each turn's once", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-1", {
      addresses: ["a1"],
      body: "Look",
      images: [png("one")],
      sessionID: "s1",
    });
    pictures.admit("m-2", {
      addresses: ["a2"],
      body: "Look",
      images: [png("two")],
      sessionID: "s1",
    });

    expect(pictures.handOver("other text")).toBeUndefined();
    expect(pictures.handOver("Look")).toEqual([png("one")]);
    expect(pictures.handOver("Look")).toEqual([png("two")]);
    expect(pictures.handOver("Look")).toBeUndefined();
  });

  test("count shown in the session they were sent into once their handed turn's message starts", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-1", {
      addresses: ["a1"],
      body: "Look",
      images: [png("one")],
      sessionID: "s1",
    });
    pictures.handOver("Look");

    expect(pictures.started({ content: "Look", role: "user", timestamp: 10 })).toBeUndefined();
    expect(pictures.started(tagged("m-other"))).toBeUndefined();
    expect(pictures.started(tagged("m-1"))).toEqual({ addresses: ["a1"], sessionID: "s1" });
    expect(pictures.started(tagged("m-1"))).toBeUndefined();
  });

  // An input handler ahead of pi-envoy's rewrote the text, so this turn's message went without
  // its pictures: they must stay unshown, and its admission is done with.
  test("never count shown for a turn whose message started without them", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-1", {
      addresses: ["a1"],
      body: "Look",
      images: [png("one")],
      sessionID: "s1",
    });

    expect(pictures.started(tagged("m-1"))).toBeUndefined();
    expect(pictures.handOver("Look")).toBeUndefined();
  });

  test("withdrawn, are handed to no input and count shown for no message", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-1", {
      addresses: ["a1"],
      body: "Look",
      images: [png("one")],
      sessionID: "s1",
    });
    pictures.admit("m-2", {
      addresses: ["a2"],
      body: "Look",
      images: [png("two")],
      sessionID: "s1",
    });
    pictures.withdraw("m-1");

    expect(pictures.handOver("Look")).toEqual([png("two")]);
    expect(pictures.started(tagged("m-1"))).toBeUndefined();
  });

  test("admitted again, replace the earlier admission and wait behind the others", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-1", {
      addresses: ["a1"],
      body: "Look",
      images: [png("old")],
      sessionID: "s1",
    });
    pictures.admit("m-2", {
      addresses: ["a2"],
      body: "Look",
      images: [png("two")],
      sessionID: "s1",
    });
    pictures.admit("m-1", {
      addresses: ["a1"],
      body: "Look",
      images: [png("new")],
      sessionID: "s1",
    });

    expect(pictures.handOver("Look")).toEqual([png("two")]);
    expect(pictures.handOver("Look")).toEqual([png("new")]);
    expect(pictures.handOver("Look")).toBeUndefined();
  });

  // The host discards every queued message when the session changes (`/new`, a resume, a
  // switch), so no message carrying an earlier session's turn will start.
  test("sent into another session are dropped when the session changes", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-old", {
      addresses: ["a1"],
      body: "Old",
      images: [png("one")],
      sessionID: "s1",
    });
    pictures.admit("m-new", {
      addresses: ["a2"],
      body: "New",
      images: [png("two")],
      sessionID: "s2",
    });
    pictures.handOver("Old");
    pictures.dropOtherSessions("s2");

    expect(pictures.started(tagged("m-old"))).toBeUndefined();
    expect(pictures.handOver("New")).toEqual([png("two")]);
    expect(pictures.started(tagged("m-new"))).toEqual({ addresses: ["a2"], sessionID: "s2" });
  });
});
