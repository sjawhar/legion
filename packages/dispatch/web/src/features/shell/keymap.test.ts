import { expect, test } from "bun:test";

import { createKeymap, DIALOG_SCOPE, type KeyBinding, type Keymap } from "./keymap";

interface Harness {
  advance: (ms: number) => void;
  keymap: Keymap;
  /** Dispatches a keydown on `target` (default: body) and reports whether a binding claimed it. */
  press: (key: string, init?: KeyboardEventInit & { target?: Element }) => boolean;
}

function harness(): Harness {
  const timers: { at: number; callback: () => void; handle: number }[] = [];
  let now = 0;
  let nextHandle = 1;
  const keymap = createKeymap({
    clearTimeout: (handle) => {
      const index = timers.findIndex((timer) => timer.handle === handle);
      if (index !== -1) timers.splice(index, 1);
    },
    setTimeout: (callback, ms) => {
      const handle = nextHandle++;
      timers.push({ at: now + ms, callback, handle });
      return handle;
    },
    strict: true,
  });
  return {
    advance: (ms) => {
      now += ms;
      const due = timers.filter((timer) => timer.at <= now);
      for (const timer of due) {
        timers.splice(timers.indexOf(timer), 1);
        timer.callback();
      }
    },
    keymap,
    press: (key, { target = document.body, ...init } = {}) => {
      const event = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key, ...init });
      target.dispatchEvent(event);
      keymap.handleKeyDown(event);
      return event.defaultPrevented;
    },
  };
}

function binding(id: string, keys: KeyBinding["keys"], extra: Partial<KeyBinding> = {}) {
  let fired = 0;
  const definition: KeyBinding = { id, keys, label: id, run: () => (fired += 1), ...extra };
  return { definition, fired: () => fired };
}

test("? matches the shifted character and $mod matches Meta or Control, never a bare key", () => {
  const { keymap, press } = harness();
  const help = binding("help", "?");
  const search = binding("search", "$mod+k");
  const pin = binding("pin", "Shift+P");
  const priority = binding("priority", "p");
  keymap.register("global", [
    help.definition,
    search.definition,
    pin.definition,
    priority.definition,
  ]);

  expect(press("?", { shiftKey: true })).toBe(true);
  expect(press("/")).toBe(false);
  expect(help.fired()).toBe(1);

  press("k", { metaKey: true });
  press("k", { ctrlKey: true });
  expect(press("k")).toBe(false);
  expect(search.fired()).toBe(2);

  press("P", { shiftKey: true });
  press("p");
  expect(pin.fired()).toBe(1);
  expect(priority.fired()).toBe(1);
});

test("a chord fires within the timeout, shows its pending prefix, and expires after 1000 ms", () => {
  const { advance, keymap, press } = harness();
  const inbox = binding("go-inbox", "g i");
  keymap.register("global", [inbox.definition]);

  expect(press("g")).toBe(true);
  expect(keymap.pending()).toBe("g");
  advance(999);
  press("i");
  expect(inbox.fired()).toBe(1);
  expect(keymap.pending()).toBe("");

  press("g");
  advance(1000);
  expect(keymap.pending()).toBe("");
  expect(press("i")).toBe(false);
  expect(inbox.fired()).toBe(1);
});

test("a key that does not continue the chord is read afresh on its own", () => {
  const { keymap, press } = harness();
  const inbox = binding("go-inbox", "g i");
  const next = binding("next", "j");
  keymap.register("global", [inbox.definition, next.definition]);

  press("g");
  press("j");
  expect(next.fired()).toBe(1);
  expect(keymap.pending()).toBe("");
});

test("registering a strict prefix of an existing sequence throws in strict mode", () => {
  const { keymap } = harness();
  keymap.register("global", [binding("go-inbox", "g i").definition]);

  expect(() => keymap.register("inbox", [binding("g", "g").definition])).toThrow(/prefix/);
  expect(() => keymap.register("global", [binding("search", "$mod+k").definition])).not.toThrow();
  expect(() => keymap.register("global", [binding("mod-k-x", "Control+k x").definition])).toThrow(
    /prefix/
  );
});

test("an open dialog masks every scope beneath it and its own bindings still fire", () => {
  const { keymap, press } = harness();
  const inbox = binding("go-inbox", "g i");
  const next = binding("next", "j");
  const close = binding("close", "Escape");
  keymap.register("global", [inbox.definition]);
  const popInbox = keymap.pushScope("inbox");
  keymap.register("inbox", [next.definition]);
  keymap.register(DIALOG_SCOPE, [close.definition]);

  const popDialog = keymap.pushScope(DIALOG_SCOPE);
  expect(press("g")).toBe(false);
  expect(press("j")).toBe(false);
  expect(press("Escape")).toBe(true);
  expect(close.fired()).toBe(1);

  popDialog();
  press("g");
  press("i");
  press("j");
  expect(inbox.fired()).toBe(1);
  expect(next.fired()).toBe(1);
  popInbox();
  expect(press("j")).toBe(false);
});

test("a page scope mounted while a dialog is open still cannot fire beneath it", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  const close = binding("close", "Escape");
  keymap.register(DIALOG_SCOPE, [close.definition]);
  keymap.register("inbox", [next.definition]);

  // `?` on /agents, then browser Back to `/`: the Inbox pushes its scope above the open dialog.
  const popDialog = keymap.pushScope(DIALOG_SCOPE);
  keymap.pushScope("inbox");
  expect(press("j")).toBe(false);
  expect(next.fired()).toBe(0);
  expect(press("Escape")).toBe(true);
  expect(close.fired()).toBe(1);

  popDialog();
  expect(press("j")).toBe(true);
  expect(next.fired()).toBe(1);
});

test("the innermost scope wins, and a binding whose when() is false lets lower scopes fire", () => {
  const { keymap, press } = harness();
  const globalOpen = binding("open-global", "o");
  const inboxOpen = binding("open-inbox", "o", { when: () => false });
  keymap.register("global", [globalOpen.definition]);
  keymap.pushScope("inbox");
  keymap.register("inbox", [inboxOpen.definition]);

  press("o");
  expect(inboxOpen.fired()).toBe(0);
  expect(globalOpen.fired()).toBe(1);
  expect(keymap.describe().map((entry) => [entry.id, entry.enabled])).toEqual([
    ["open-global", true],
    ["open-inbox", false],
  ]);
});

test("only inEditable bindings fire while an input, textarea, select, or contentEditable has focus", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  const search = binding("search", "$mod+k", { inEditable: true });
  keymap.register("global", [next.definition, search.definition]);

  const editables = ["input", "textarea", "select"].map((tag) => document.createElement(tag));
  const editor = document.createElement("div");
  editor.contentEditable = "true";
  editables.push(editor);
  const button = document.createElement("button");
  for (const element of [...editables, button]) {
    document.body.appendChild(element);
  }
  try {
    for (const target of editables) {
      expect(press("j", { target })).toBe(false);
      expect(press("k", { ctrlKey: true, target })).toBe(true);
    }
    expect(next.fired()).toBe(0);
    expect(search.fired()).toBe(editables.length);
    expect(press("j", { target: button })).toBe(true);
    expect(next.fired()).toBe(1);
  } finally {
    for (const element of [...editables, button]) {
      element.remove();
    }
  }
});

test("a reserved binding never fires but is described with its reason", () => {
  const { keymap, press } = harness();
  const select = binding("select", "x", { reserved: "no bulk action yet" });
  keymap.register("inbox", [select.definition]);
  keymap.pushScope("inbox");

  expect(press("x")).toBe(false);
  expect(select.fired()).toBe(0);
  expect(keymap.describe()).toEqual([
    {
      enabled: false,
      id: "select",
      keys: ["x"],
      label: "select",
      reserved: "no bulk action yet",
      scope: "inbox",
    },
  ]);
});

test("an already-handled key (defaultPrevented) or one mid-IME-composition is left alone", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  keymap.register("global", [next.definition]);
  const swallow = (event: Event) => event.preventDefault();
  document.body.addEventListener("keydown", swallow);
  try {
    press("j");
    expect(next.fired()).toBe(0);
  } finally {
    document.body.removeEventListener("keydown", swallow);
  }
  expect(press("j", { isComposing: true })).toBe(false);
  expect(next.fired()).toBe(0);
  press("j");
  expect(next.fired()).toBe(1);
});
