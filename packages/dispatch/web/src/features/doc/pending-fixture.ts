import * as Y from "yjs";

import { openPendingEdits, type PendingEdits, type PendingKey } from "./pending-edits";

// The frames a Hocuspocus peer exchanges: the document name, a message tag, then the message.
// Built by hand here so the expected bytes never come from the code under test.
function varUint(value: number): number[] {
  const bytes: number[] = [];
  let rest = value;
  while (rest >= 0x80) {
    bytes.push((rest & 0x7f) | 0x80);
    rest = Math.floor(rest / 0x80);
  }
  bytes.push(rest);
  return bytes;
}
function varBytes(value: Uint8Array): number[] {
  return [...varUint(value.length), ...value];
}
export function frame(name: string, ...parts: number[][]): Uint8Array {
  return Uint8Array.from([...varBytes(new TextEncoder().encode(name)), ...parts.flat()]);
}
export const NAME = "the-document";
export const updateFrame = (update: Uint8Array) => frame(NAME, [0, 2], varBytes(update));
export const syncStep2Frame = (update: Uint8Array, name = NAME) =>
  frame(name, [0, 1], varBytes(update));
export const syncStep1Frame = (stateVector: Uint8Array, name: string) =>
  frame(name, [0, 0], varBytes(stateVector));
export const syncStatusFrame = (applied: boolean, name = NAME) => frame(name, [8, applied ? 1 : 0]);
export const authenticatedFrame = (scope: string, name: string) =>
  frame(name, [2, 2], varBytes(new TextEncoder().encode(scope)));
/** An awareness frame naming no client, as a room no other peer is in sends it. */
export const awarenessFrame = (name: string) => frame(name, [1], varBytes(Uint8Array.of(0)));

let next = 0;
export function artifactId(): string {
  next += 1;
  return `pending-test-${next}`;
}

const openStores: PendingEdits[] = [];

/** Closes every store `storeFor` opened; a test file runs it after each test. */
export function closeStores(): void {
  for (const store of openStores.splice(0)) {
    store.close();
  }
}

export async function storeFor(artifact: string, clientId: number): Promise<PendingEdits> {
  const store = await openPendingEdits(artifact, clientId);
  if (store === undefined) {
    throw new Error("fake-indexeddb refused to open");
  }
  openStores.push(store);
  return store;
}

/** The keys a document's rows hold, read through a store of a client that wrote none. */
export async function rowsOf(artifact: string): Promise<PendingKey[]> {
  return (await (await storeFor(artifact, -1)).restoreRows()).map((row) => row.key);
}

/**
 * A browser of an earlier page load: it applied `server`, made each of `changes` in turn, and kept
 * each as a row of its own, as the edits of one page load are kept.
 */
export async function savedChanges(
  artifact: string,
  server: Y.Doc,
  ...changes: ((browser: Y.Doc) => void)[]
): Promise<{ key: PendingKey; update: Uint8Array }[]> {
  const browser = new Y.Doc();
  Y.applyUpdate(browser, Y.encodeStateAsUpdate(server));
  const store = await storeFor(artifact, browser.clientID);
  const saved: { key: PendingKey; update: Uint8Array }[] = [];
  for (const change of changes) {
    let update: Uint8Array | undefined;
    const capture = (emitted: Uint8Array) => {
      update = emitted;
    };
    browser.on("update", capture);
    change(browser);
    browser.off("update", capture);
    if (update === undefined) {
      throw new Error("a local edit emits an update");
    }
    const row = store.record(update);
    await row.written;
    saved.push({ key: [artifact, browser.clientID, row.seq], update });
  }
  return saved;
}

/** A browser of an earlier page load: it applied `server`, typed `text`, and kept the edit. */
export async function savedEdit(
  artifact: string,
  server: Y.Doc,
  text: string
): Promise<Uint8Array> {
  const [saved] = await savedChanges(artifact, server, (browser) => typeInto(browser, text));
  if (saved === undefined) {
    throw new Error("one change saves one row");
  }
  return saved.update;
}

/** A server document holding one heading whose `id` is empty, as the server writes a heading. */
export function serverWithHeading(clientId: number): Y.Doc {
  const server = new Y.Doc();
  server.clientID = clientId;
  const heading = new Y.XmlElement("heading");
  heading.setAttribute("id", "");
  heading.insert(0, [new Y.XmlText("Plan")]);
  server.getXmlFragment("prosemirror").insert(0, [heading]);
  return server;
}

/** Stamps the heading's id from its text, as the editor's heading-id plugin does on every load. */
export function stampHeadingId(doc: Y.Doc): void {
  const heading = doc.getXmlFragment("prosemirror").get(0);
  if (!(heading instanceof Y.XmlElement)) {
    throw new Error("the document's first block is a heading");
  }
  heading.setAttribute("id", "plan");
}

/** Types a paragraph after the heading, as a person does in a writable document. */
export function typeParagraph(doc: Y.Doc): void {
  const paragraph = new Y.XmlElement("paragraph");
  paragraph.insert(0, [new Y.XmlText("Typed.")]);
  doc.getXmlFragment("prosemirror").insert(1, [paragraph]);
}

/** The text of the document's first paragraph, where these tests type. */
export function paragraphText(doc: Y.Doc): Y.XmlText {
  const paragraph = doc.getXmlFragment("prosemirror").get(0);
  const text = paragraph instanceof Y.XmlElement ? paragraph.get(0) : undefined;
  if (!(text instanceof Y.XmlText)) {
    throw new Error("the document's first block is a paragraph of text");
  }
  return text;
}

/** Types `text` at the end of the first paragraph, opening one in an empty document. */
export function typeInto(doc: Y.Doc, text: string): void {
  const fragment = doc.getXmlFragment("prosemirror");
  if (fragment.length === 0) {
    const paragraph = new Y.XmlElement("paragraph");
    paragraph.insert(0, [new Y.XmlText(text)]);
    fragment.insert(0, [paragraph]);
    return;
  }
  const typed = paragraphText(doc);
  typed.insert(typed.length, text);
}

/** A server document holding one paragraph of `text`. */
export function serverWith(text: string, clientId: number): Y.Doc {
  const server = new Y.Doc();
  server.clientID = clientId;
  typeInto(server, text);
  return server;
}
