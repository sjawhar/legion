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

  test("count shown once their handed turn's message starts", () => {
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
    expect(pictures.started(tagged("m-1"))).toEqual(["a1"]);
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

  // The host has run every input handler once `sendUserInput` answers that it submitted the turn:
  // a turn still holding its pictures then had its text rewritten before pi-envoy's handler, and
  // will never be handed them. A turn handed them waits for its message to start.
  test("never handed over are released once the input has passed every handler", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-rewritten", {
      addresses: ["a1"],
      body: "Look",
      images: [png("one")],
      sessionID: "s1",
    });
    pictures.admit("m-handed", {
      addresses: ["a2"],
      body: "Other",
      images: [png("two")],
      sessionID: "s1",
    });
    pictures.handOver("Other");
    pictures.inputPassed("m-rewritten");
    pictures.inputPassed("m-handed");

    expect(pictures.handOver("Look")).toBeUndefined();
    expect(pictures.started(tagged("m-rewritten"))).toBeUndefined();
    expect(pictures.started(tagged("m-handed"))).toEqual(["a2"]);
  });

  // A switch, `/new` or resume discards the queue of the session left, so a turn sent into it and
  // already handed its pictures never starts. One still holding them is in a `sendUserInput` that
  // has not reached pi-envoy's input handler yet, which will still ask for them.
  test("sent into another session go when the session changes, unless still waiting to be handed over", () => {
    const pictures = new TypedPictures();
    pictures.admit("m-handed", {
      addresses: ["a1"],
      body: "Old",
      images: [png("one")],
      sessionID: "s1",
    });
    pictures.admit("m-waiting", {
      addresses: ["a2"],
      body: "Waiting",
      images: [png("two")],
      sessionID: "s1",
    });
    pictures.handOver("Old");
    pictures.dropOtherSessions("s2");

    expect(pictures.started(tagged("m-handed"))).toBeUndefined();
    expect(pictures.handOver("Waiting")).toEqual([png("two")]);
    expect(pictures.started(tagged("m-waiting"))).toEqual(["a2"]);
  });
});
