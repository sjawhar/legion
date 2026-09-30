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

test("a checkbox, radio or button takes no text, so single keys still fire while one has focus", () => {
  const { keymap, press } = harness();
  const next = binding("next", "j");
  keymap.register("global", [next.definition]);

  const typed = (type: string) => {
    const input = document.createElement("input");
    input.type = type;
    return input;
  };
  const shortcutTargets = [typed("checkbox"), typed("radio"), typed("button")];
  const typing = [typed("text"), typed("search"), typed("unknown-becomes-text")];
  for (const element of [...shortcutTargets, ...typing]) {
    document.body.appendChild(element);
  }
  try {
    for (const target of shortcutTargets) {
      expect(press("j", { target })).toBe(true);
    }
    expect(next.fired()).toBe(shortcutTargets.length);
    for (const target of typing) {
      expect(press("j", { target })).toBe(false);
    }
    expect(next.fired()).toBe(shortcutTargets.length);
  } finally {
    for (const element of [...shortcutTargets, ...typing]) {
      element.remove();
    }
  }
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

test("project and board are page scopes: their bindings are described under their own scope and the innermost wins", () => {
  const { keymap, press } = harness();
  const toggleView = binding("toggle-view", "v");
  const nextCard = binding("next", "j");
  keymap.register("project", [toggleView.definition]);
  keymap.register("board", [nextCard.definition]);
  keymap.pushScope("project");
  keymap.pushScope("board");

  expect(keymap.describe().map((entry) => [entry.scope, entry.id])).toEqual([
    ["project", "toggle-view"],
    ["board", "next"],
  ]);
  expect(press("v")).toBe(true);
  expect(toggleView.fired()).toBe(1);
  expect(press("j")).toBe(true);
  expect(nextCard.fired()).toBe(1);
});

test("actions() lists each scope below the innermost dialog once, innermost scope first", () => {
  const { keymap } = harness();
  const create = binding("create", "c");
  const toggleView = binding("toggle-view", "v");
  const closePalette = binding("close-palette", "Escape", { inEditable: true });
  keymap.register("global", [create.definition]);
  keymap.register("project", [toggleView.definition]);
  keymap.register(DIALOG_SCOPE, [closePalette.definition]);
  // A page can push its scope twice (a panel kept mounted while hidden); one row all the same.
  keymap.pushScope("project");
  keymap.pushScope("project");
  keymap.pushScope(DIALOG_SCOPE);

  expect(keymap.actions().map((action) => [action.scope, action.id])).toEqual([
    ["project", "toggle-view"],
    ["global", "create"],
  ]);
});

test("actions() orders a scope's rows by label, never by when its component registered them", () => {
  const { keymap } = harness();
  // Registration order is mount timing: the same page reached by a navigation and by a reload
  // registers its scopes in a different order, and the highlighted first row must not follow it.
  keymap.register("issue", [binding("set-p1", [], { label: "Set priority P1" }).definition]);
  keymap.register("global", [binding("go-inbox", "g i", { label: "Go to Inbox" }).definition]);
  keymap.register("issue", [binding("close", [], { label: "Close issue" }).definition]);
  keymap.register("global", [binding("create", "c", { label: "Create issue" }).definition]);
  keymap.pushScope("issue");

  expect(keymap.actions().map((action) => action.label)).toEqual([
    "Close issue",
    "Set priority P1",
    "Create issue",
    "Go to Inbox",
  ]);
});

test("actions() omits palette:false, multi-key, inEditable and unavailable bindings", () => {
  const { keymap } = harness();
  const next = binding("next", "j", { palette: false });
  const arrows = binding("arrows", ["ArrowDown", "ArrowUp"]);
  const search = binding("search", "$mod+k", { inEditable: true });
  const move = binding("move", "Shift+J", { when: () => false });
  const open = binding("open", "o");
  keymap.register("board", [
    next.definition,
    arrows.definition,
    search.definition,
    move.definition,
    open.definition,
  ]);
  keymap.pushScope("board");

  expect(keymap.actions().map((action) => action.id)).toEqual(["open"]);
});

test("a binding with several keys is a row only when it says so, and the row presses its first key", () => {
  const { keymap } = harness();
  let pressed = "";
  const ascend = binding("ascend", ["h", "ArrowLeft"], {
    label: "Up one level",
    palette: true,
    run: (event) => {
      pressed = event.key;
    },
  });
  const arrows = binding("arrows", ["ArrowDown", "ArrowUp"]);
  keymap.register("architecture", [ascend.definition, arrows.definition]);
  keymap.pushScope("architecture");

  const rows = keymap.actions();
  expect(rows.map((action) => action.id)).toEqual(["ascend"]);
  rows[0]?.run();
  expect(pressed).toBe("h");
});

test("a keyless binding never fires on a key press yet is offered as an action", () => {
  const { keymap, press } = harness();
  const close = binding("close", [], { label: "Close issue" });
  keymap.register("issue", [close.definition]);
  keymap.pushScope("issue");

  expect(press("c")).toBe(false);
  expect(press("Enter")).toBe(false);
  expect(close.fired()).toBe(0);
  expect(keymap.describe().map((entry) => [entry.id, entry.keys])).toEqual([["close", []]]);
  expect(keymap.actions()).toMatchObject([
    { id: "close", keys: [], label: "Close issue", scope: "issue" },
  ]);
});

test("running an action fires its binding once with a keydown carrying its first key", () => {
  const { keymap } = harness();
  const fired: string[] = [];
  keymap.register("global", [
    { id: "snooze", keys: "s", label: "Snooze", run: (event) => fired.push(`snooze:${event.key}`) },
    {
      id: "close",
      keys: [],
      label: "Close issue",
      run: (event) => fired.push(`close:${event.key}`),
    },
  ]);

  for (const action of keymap.actions()) {
    action.run();
  }

  // Sorted, so the assertion is about which key each binding received and that each ran once,
  // not about the row order `actions()` chose (pinned by its own test above).
  expect([...fired].sort()).toEqual(["close:", "snooze:s"]);
});

test("a row runs its binding only if the binding still applies when the row is chosen", () => {
  const { keymap } = harness();
  let closed = true;
  const reopen = binding("reopen", [], { label: "Reopen issue", when: () => closed });
  keymap.register("issue", [reopen.definition]);
  keymap.pushScope("issue");
  const [row] = keymap.actions();

  // The issue was reopened elsewhere while the palette was open: the row's control has gone.
  closed = false;
  row?.run();
  expect(reopen.fired()).toBe(0);

  closed = true;
  row?.run();
  expect(reopen.fired()).toBe(1);
});

test("a row does nothing once its binding's component has unregistered it", () => {
  const { keymap } = harness();
  const close = binding("close", [], { label: "Close issue" });
  const unregister = keymap.register("issue", [close.definition]);
  keymap.pushScope("issue");
  const [row] = keymap.actions();

  // The issue header unmounted while the palette was open (the page fell to its error view).
  unregister();
  row?.run();
  expect(close.fired()).toBe(0);
});

test("rows within a scope read alphabetically, a lowercase word among capitalised ones included", () => {
  const { keymap } = harness();
  keymap.register("global", [
    binding("go-settings", "g s", { label: "Go to Settings" }).definition,
    binding("go-project", "g p", { label: "Go to project…" }).definition,
    binding("go-inbox", "g i", { label: "Go to Inbox" }).definition,
  ]);

  expect(keymap.actions().map((action) => action.label)).toEqual([
    "Go to Inbox",
    "Go to project…",
    "Go to Settings",
  ]);
});
