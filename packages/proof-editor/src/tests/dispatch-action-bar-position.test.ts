// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import { test } from './harness.js';
import { dispatchActionBarPlugin, positionBar } from '../dispatch-action-bar.js';
import type { MarkRange } from 'proof-sdk-upstream/src/editor/plugins/marks.js';
import type { EditorView } from '@milkdown/kit/prose/view';

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

function withMockWindow<T>(windowLike: { innerWidth: number; innerHeight: number }, fn: () => T): T {
  const g = globalThis as { window?: unknown };
  const prevWindow = g.window;
  g.window = windowLike;
  try {
    return fn();
  } finally {
    if (prevWindow === undefined) {
      delete g.window;
    } else {
      g.window = prevWindow;
    }
  }
}

type Listener = (event: Event) => void;

class FakeElement {
  className = '';
  type = '';
  textContent = '';
  innerHTML = '';
  parentElement: FakeElement | null = null;
  readonly children: FakeElement[] = [];
  readonly dataset: Record<string, string> = {};
  readonly style = {} as CSSStyleDeclaration;
  private readonly listeners = new Map<string, Listener[]>();

  addEventListener(type: string, listener: Listener): void {
    const listeners = this.listeners.get(type) ?? [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: Listener): void {
    const listeners = this.listeners.get(type);
    if (listeners === undefined) return;
    this.listeners.set(
      type,
      listeners.filter((candidate) => candidate !== listener),
    );
  }

  dispatch(type: string, event = {} as Event): void {
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }

  appendChild<T extends FakeElement>(child: T): T {
    child.parentElement = this;
    this.children.push(child);
    return child;
  }

  contains(node: unknown): boolean {
    return node === this || this.children.some((child) => child.contains(node));
  }

  remove(): void {
    if (!this.parentElement) return;
    const index = this.parentElement.children.indexOf(this);
    if (index >= 0) this.parentElement.children.splice(index, 1);
    this.parentElement = null;
  }

  getBoundingClientRect() {
    return { width: 120, height: 40, top: 0, bottom: 40, left: 0, right: 120 };
  }
}

class FakeDocument {
  readonly body = new FakeElement();
  activeElement: FakeElement | null = null;
  private readonly listeners = new Map<string, Listener[]>();

  createElement(): FakeElement {
    return new FakeElement();
  }

  addEventListener(type: string, listener: Listener): void {
    const listeners = this.listeners.get(type) ?? [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: Listener): void {
    const listeners = this.listeners.get(type);
    if (listeners === undefined) return;
    this.listeners.set(
      type,
      listeners.filter((candidate) => candidate !== listener),
    );
  }

  dispatch(type: string): void {
    for (const listener of this.listeners.get(type) ?? []) listener({} as Event);
  }
}

function createActionBarView(document: FakeDocument) {
  const dom = new FakeElement();
  document.body.appendChild(dom);
  const selection: Box = { top: 200, bottom: 220, left: 379, right: 600 };
  const state = {
    selection: { from: 1, to: 2 },
    doc: { content: { size: 3 } },
  };
  const view = {
    dom,
    state,
    coordsAtPos: () => selection,
    hasFocus: () => true,
  } as unknown as EditorView;

  return {
    dom,
    view,
    setSelection: (from: number, to: number) => {
      state.selection = { from, to };
    },
  };
}

function installActionBarDom(coarse: boolean) {
  const previousWindow = (globalThis as { window?: unknown }).window;
  const previousDocument = (globalThis as { document?: unknown }).document;
  const previousSetTimeout = globalThis.setTimeout;
  const previousClearTimeout = globalThis.clearTimeout;
  const document = new FakeDocument();
  const timers = new Map<number, () => void>();
  let lastTimer: (() => void) | null = null;
  let nextTimer = 1;

  const windowLike = {
    innerWidth: 1_600,
    innerHeight: 900,
    matchMedia: () => ({ matches: coarse }),
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
  };
  (globalThis as { window: unknown }).window = windowLike;
  (globalThis as { document: unknown }).document = document;
  globalThis.setTimeout = ((callback: TimerHandler) => {
    if (typeof callback !== 'function') throw new Error('Expected a function timer callback');
    lastTimer = callback;
    const timer = nextTimer++;
    timers.set(timer, callback);
    return timer;
  }) as unknown as typeof setTimeout;
  globalThis.clearTimeout = ((timer: number) => {
    timers.delete(timer);
  }) as unknown as typeof clearTimeout;

  const primary = createActionBarView(document);
  document.activeElement = primary.dom;

  return {
    document,
    view: primary.view,
    actionBar: () => document.body.children.find((child) => child.className === 'dispatch-action-bar'),
    actionBars: () => document.body.children.filter((child) => child.className === 'dispatch-action-bar'),
    setSelection: primary.setSelection,
    createView: () => createActionBarView(document),
    runTimers: () => {
      const pending = [...timers.values()];
      timers.clear();
      for (const timer of pending) timer();
    },
    runLastTimer: () => {
      lastTimer?.();
    },
    restore: () => {
      globalThis.setTimeout = previousSetTimeout;
      globalThis.clearTimeout = previousClearTimeout;
      if (previousWindow === undefined) delete (globalThis as { window?: unknown }).window;
      else (globalThis as { window: unknown }).window = previousWindow;
      if (previousDocument === undefined) delete (globalThis as { document?: unknown }).document;
      else (globalThis as { document: unknown }).document = previousDocument;
    },
  };
}

async function createActionBarController(view: EditorView) {
  const actionBarPlugin = dispatchActionBarPlugin({ by: 'Ada' });
  await actionBarPlugin({
    wait: async () => undefined,
    update: () => undefined,
  } as never)();
  const controller = actionBarPlugin.plugin().spec.view?.(view);
  assert(controller !== undefined, 'Expected action bar plugin to create a controller');
  return controller as { update(view: EditorView): void; destroy(): void };
}

type Box = { top: number; bottom: number; left: number; right: number };

function makeBar(rect: { width: number; height: number }): HTMLElement {
  const style: Record<string, string> = {};
  return {
    style,
    getBoundingClientRect: () => ({ ...rect, top: 0, bottom: rect.height, left: 0, right: rect.width }),
  } as unknown as HTMLElement;
}

// The editor rect only bounds the bar vertically (it must never rise above the
// editor's top edge); there is no gutter to dock into.
function makeView(coords: { from: Box; to: Box }, editor: Box = { top: 0, bottom: 10_000, left: 0, right: 10_000 }): EditorView {
  return {
    dom: { getBoundingClientRect: () => editor },
    coordsAtPos: (pos: number) => (pos === 0 ? coords.from : coords.to),
  } as unknown as EditorView;
}

console.log('\n=== dispatch-action-bar positionBar ===');

await test('centers the bar on the selection, above it, on a wide viewport with room on both sides', () => {
  // A selection from x 379-600 sitting comfortably inside a 1600px viewport —
  // the gutter-docking branch used to place the bar at a fixed offset past
  // the editor's right edge regardless of where the selection was; it must
  // now land near the selection instead.
  const selection: Box = { top: 200, bottom: 220, left: 379, right: 600 };
  const view = makeView({ from: selection, to: selection });
  const bar = makeBar({ width: 120, height: 40 });
  const range: MarkRange = { from: 0, to: 1 };

  withMockWindow({ innerWidth: 1600, innerHeight: 900 }, () => {
    positionBar(bar, view, range);
  });

  const barLeft = parseFloat(bar.style.left);
  const barTop = parseFloat(bar.style.top);
  const barWidth = 120;
  const barHeight = 40;
  const selectionCenterX = (selection.left + selection.right) / 2;
  const barCenterX = barLeft + barWidth / 2;

  assert(
    Math.abs(barCenterX - selectionCenterX) <= 8,
    `Expected bar center x within 8px of selection center x (${selectionCenterX}), got ${barCenterX}`,
  );
  assert(
    barTop + barHeight <= selection.top,
    `Expected the bar to sit above the selection (bar bottom ${barTop + barHeight} <= selection top ${selection.top})`,
  );
});

await test('never docks in the gutter even when there is ample room past where an editor edge would be', () => {
  // Previously: spaceRight/spaceLeft measured against view.dom's rect, and a
  // wide gap on either side triggered docking at editorRect.right/left +
  // DOCK_GAP. The view here has no dom rect at all — if positionBar tried to
  // read one, this would throw inside the try/catch and leave bar.style
  // unset, which the assertions below would catch.
  const selection: Box = { top: 300, bottom: 320, left: 40, right: 90 };
  const view = makeView({ from: selection, to: selection });
  const bar = makeBar({ width: 100, height: 32 });
  const range: MarkRange = { from: 0, to: 1 };

  withMockWindow({ innerWidth: 1600, innerHeight: 900 }, () => {
    positionBar(bar, view, range);
  });

  assert(bar.style.left !== undefined && bar.style.left !== '', 'Expected positionBar to set a left offset');
  const barLeft = parseFloat(bar.style.left);
  const selectionCenterX = (selection.left + selection.right) / 2;
  assert(
    Math.abs(barLeft + 50 - selectionCenterX) <= 8,
    `Expected the bar centered near the selection (left edge), got left=${barLeft}`,
  );
});

await test('falls back to below the selection when there is no room above', () => {
  const selection: Box = { top: 5, bottom: 25, left: 700, right: 900 };
  const view = makeView({ from: selection, to: selection });
  const bar = makeBar({ width: 120, height: 40 });
  const range: MarkRange = { from: 0, to: 1 };

  withMockWindow({ innerWidth: 1600, innerHeight: 900 }, () => {
    positionBar(bar, view, range);
  });

  const barTop = parseFloat(bar.style.top);
  assert(barTop >= selection.bottom, `Expected the bar below the selection, got top=${barTop}`);
});

await test('never rises above the editor: a first-line selection puts the bar below it, not over the host chrome', () => {
  // Host layout: tabs at y 175-219, editor starting at y 230, selection on the first line.
  const selection: Box = { top: 235, bottom: 254, left: 427, right: 470 };
  const editor: Box = { top: 230, bottom: 900, left: 344, right: 1192 };
  const view = makeView({ from: selection, to: selection }, editor);
  const bar = makeBar({ width: 195, height: 36 });
  const range: MarkRange = { from: 0, to: 1 };

  withMockWindow({ innerWidth: 1280, innerHeight: 720 }, () => {
    positionBar(bar, view, range);
  });

  const barTop = parseFloat(bar.style.top);
  assert(barTop >= editor.top, `Expected the bar inside the editor (top >= ${editor.top}), got top=${barTop}`);
  assert(barTop >= selection.bottom, `Expected the bar below the first-line selection, got top=${barTop}`);
});

await test('coarse-pointer selections dock after the selection settles', async () => {
  const fixture = installActionBarDom(true);
  try {
    const controller = await createActionBarController(fixture.view);
    controller.update(fixture.view);
    const bar = fixture.actionBar();
    assert(bar !== undefined, 'Expected action bar element to be attached');
    assert(bar.style.display === 'none', 'Expected touch selection bar to stay hidden before selection settles');

    fixture.document.dispatch('selectionchange');
    assert(bar.style.display === 'none', 'Expected touch selection bar to remain hidden during its debounce');
    fixture.runTimers();

    assert(bar.style.display === 'flex', 'Expected touch selection bar to show after its debounce');
    assert(bar.dataset.touch === 'true', 'Expected coarse-pointer selection bar to expose data-touch=true');
    assert(!bar.style.top, 'Expected bottom-docked touch selection bar to have no inline top offset');

    fixture.setSelection(1, 1);
    controller.update(fixture.view);
    assert(bar.style.display === 'none', 'Expected touch selection bar to hide when the selection collapses');
    controller.destroy();
  } finally {
    fixture.restore();
  }
});

await test('fine-pointer selections still appear immediately above their selection', async () => {
  const fixture = installActionBarDom(false);
  try {
    const controller = await createActionBarController(fixture.view);
    controller.update(fixture.view);
    const bar = fixture.actionBar();
    assert(bar !== undefined, 'Expected action bar element to be attached');
    assert(bar.style.display === 'flex', 'Expected fine-pointer selection bar to show immediately');
    assert(bar.dataset.touch === undefined, 'Expected fine-pointer selection bar to remain unmarked as touch');
    assert(
      Number.parseFloat(bar.style.top) + 40 <= 200,
      `Expected fine-pointer selection bar above the selection, got top=${bar.style.top}`,
    );
    controller.destroy();
  } finally {
    fixture.restore();
  }
});
await test('fine-pointer selectionchange leaves two action bars to their editor transactions', async () => {
  const fixture = installActionBarDom(false);
  try {
    const firstController = await createActionBarController(fixture.view);
    firstController.update(fixture.view);
    const peer = fixture.createView();
    const secondController = await createActionBarController(peer.view);
    secondController.update(peer.view);
    const [firstBar, secondBar] = fixture.actionBars();
    assert(firstBar !== undefined && secondBar !== undefined, 'Expected both editor views to have action bars');
    const firstTop = firstBar.style.top;
    const secondTop = secondBar.style.top;

    fixture.setSelection(1, 1);
    fixture.document.dispatch('selectionchange');

    assert(firstBar.style.display === 'flex', 'Expected the first fine-pointer bar to remain unchanged');
    assert(secondBar.style.display === 'flex', 'Expected the peer fine-pointer bar to remain unchanged');
    assert(firstBar.style.top === firstTop, 'Expected the first fine-pointer bar position to remain unchanged');
    assert(secondBar.style.top === secondTop, 'Expected the peer fine-pointer bar position to remain unchanged');
    firstController.destroy();
    secondController.destroy();
  } finally {
    fixture.restore();
  }
});

await test('hides the selection bar when focus leaves the editor', async () => {
  const fixture = installActionBarDom(true);
  try {
    const controller = await createActionBarController(fixture.view);
    controller.update(fixture.view);
    fixture.runTimers();
    const bar = fixture.actionBar();
    assert(bar !== undefined, 'Expected action bar element to be attached');
    assert(bar.style.display === 'flex', 'Expected the settled touch selection bar to be visible');

    const editorDom = fixture.view.dom as unknown as FakeElement;
    editorDom.dispatch('focusout', { relatedTarget: fixture.document.body } as FocusEvent);
    assert(bar.style.display === 'none', 'Expected selection bar to hide when the editor loses focus');
    controller.destroy();
  } finally {
    fixture.restore();
  }
});
await test('fine-pointer focus loss leaves the action bar visible until its editor update', async () => {
  const fixture = installActionBarDom(false);
  try {
    const controller = await createActionBarController(fixture.view);
    controller.update(fixture.view);
    const bar = fixture.actionBar();
    assert(bar !== undefined, 'Expected action bar element to be attached');

    const editorDom = fixture.view.dom as unknown as FakeElement;
    editorDom.dispatch('focusout', { relatedTarget: fixture.document.body } as FocusEvent);
    assert(bar.style.display === 'flex', 'Expected fine-pointer focus loss to leave the action bar unchanged');
    controller.destroy();
  } finally {
    fixture.restore();
  }
});

await test('a mouse selection overrides a coarse-pointer media query', async () => {
  const fixture = installActionBarDom(true);
  try {
    const controller = await createActionBarController(fixture.view);
    controller.update(fixture.view);
    const bar = fixture.actionBar();
    assert(bar !== undefined, 'Expected action bar element to be attached');

    const editorDom = fixture.view.dom as unknown as FakeElement;
    editorDom.dispatch('pointerdown', { pointerType: 'mouse' } as PointerEvent);
    fixture.document.dispatch('pointerup');
    controller.update(fixture.view);

    assert(bar.style.display === 'flex', 'Expected a known mouse selection bar to appear immediately');
    assert(bar.dataset.touch === undefined, 'Expected a known mouse selection to clear data-touch');
    assert(Number.parseFloat(bar.style.top) + 40 <= 200, 'Expected a known mouse selection bar above the text');
    controller.destroy();
  } finally {
    fixture.restore();
  }
});

await test('a cancelled touch debounce cannot show a destroyed action bar', async () => {
  const fixture = installActionBarDom(true);
  try {
    const controller = await createActionBarController(fixture.view);
    controller.update(fixture.view);
    const bar = fixture.actionBar();
    assert(bar !== undefined, 'Expected action bar element to be attached');

    controller.destroy();
    fixture.runLastTimer();
    assert(bar.style.display === 'none', 'Expected a destroyed action bar to remain hidden');
  } finally {
    fixture.restore();
  }
});

