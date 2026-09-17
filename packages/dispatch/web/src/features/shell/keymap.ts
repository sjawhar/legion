import { useLayoutEffect, useRef, useSyncExternalStore } from "react";

/**
 * One keyboard registry for the SPA. Components register bindings under a scope
 * (`useKeymap`), pages and dialogs push their scope onto the shell-owned stack
 * (`useKeymapScope`), and a single `window` keydown listener (`KeymapProvider`) resolves each
 * key top-down through the stack: the first scope holding a matching binding wins, and while
 * a `dialog` scope is on the stack it is the only scope consulted. `keys` follows tinykeys' string
 * syntax — `"g i"` is a chord, `"$mod+k"` matches Meta or Control, `"Shift+P"` a shifted
 * letter, `"?"` the character an unshifted or shifted key produces.
 */

export type KeymapScope = "global" | "dialog" | "inbox" | "project" | "architecture" | "board";

export interface KeyBinding {
  /** Stable identifier, unique within the registering component. */
  id: string;
  /** One or more alternatives in tinykeys syntax, e.g. `["j", "ArrowDown"]`. */
  keys: string | readonly string[];
  label: string;
  run: (event: KeyboardEvent) => void;
  /** Whether the binding applies right now; `false` neither fires nor shadows lower scopes. */
  when?: () => boolean;
  /** Fires while an `INPUT`, `TEXTAREA`, `SELECT`, or contentEditable element has focus. */
  inEditable?: boolean;
}

/** A binding as `?` describes it: keys as typed, its scope, and whether it applies right now. */
export interface KeyBindingDescription {
  enabled: boolean;
  id: string;
  keys: readonly string[];
  label: string;
  scope: KeymapScope;
}

export const DIALOG_SCOPE: KeymapScope = "dialog";
const CHORD_TIMEOUT_MS = 1000;

interface KeyCombo {
  alt: boolean;
  control: boolean;
  key: string;
  meta: boolean;
  mod: boolean;
  shift: boolean;
}

interface KeyPress {
  altKey: boolean;
  ctrlKey: boolean;
  key: string;
  metaKey: boolean;
  shiftKey: boolean;
}

interface Registration {
  binding: KeyBinding;
  chords: readonly (readonly KeyCombo[])[];
  scope: KeymapScope;
}

const MODIFIER_KEYS: Record<string, true> = {
  Alt: true,
  AltGraph: true,
  Control: true,
  Meta: true,
  Shift: true,
};

function parseCombo(text: string): KeyCombo {
  const parts = text.split("+");
  const key = parts.pop();
  if (key === undefined || key === "") {
    throw new Error(`Keymap: "${text}" has no key`);
  }
  const combo: KeyCombo = {
    alt: false,
    control: false,
    key,
    meta: false,
    mod: false,
    shift: false,
  };
  for (const part of parts) {
    switch (part) {
      case "$mod":
        combo.mod = true;
        break;
      case "Control":
      case "Ctrl":
        combo.control = true;
        break;
      case "Meta":
        combo.meta = true;
        break;
      case "Alt":
        combo.alt = true;
        break;
      case "Shift":
        combo.shift = true;
        break;
      default:
        throw new Error(`Keymap: unknown modifier "${part}" in "${text}"`);
    }
  }
  return combo;
}

function keysOf(binding: KeyBinding): readonly string[] {
  return typeof binding.keys === "string" ? [binding.keys] : binding.keys;
}

function matchesCombo(combo: KeyCombo, press: KeyPress): boolean {
  const sameKey =
    press.key.length === 1
      ? press.key.toLowerCase() === combo.key.toLowerCase()
      : press.key === combo.key;
  if (!sameKey || combo.alt !== press.altKey) {
    return false;
  }
  // A single non-letter character (`?`, `/`, `1`) already encodes whether Shift was held.
  const shiftEncodedInKey =
    combo.key.length === 1 && combo.key.toLowerCase() === combo.key.toUpperCase();
  if (!shiftEncodedInKey && combo.shift !== press.shiftKey) {
    return false;
  }
  if (combo.mod) {
    return press.metaKey || press.ctrlKey;
  }
  return combo.control === press.ctrlKey && combo.meta === press.metaKey;
}

/** Whether two combos can be produced by one key press (`$mod+k` overlaps `Control+k`). */
function sameCombo(a: KeyCombo, b: KeyCombo): boolean {
  if (a.key.toLowerCase() !== b.key.toLowerCase() || a.alt !== b.alt || a.shift !== b.shift) {
    return false;
  }
  if (a.mod || b.mod) {
    return (a.mod || a.control || a.meta) && (b.mod || b.control || b.meta);
  }
  return a.control === b.control && a.meta === b.meta;
}

function isStrictPrefix(shorter: readonly KeyCombo[], longer: readonly KeyCombo[]): boolean {
  return (
    shorter.length < longer.length &&
    shorter.every((combo, index) => sameCombo(combo, longer[index] as KeyCombo))
  );
}

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  const tag = target.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.isContentEditable;
}

export interface KeymapOptions {
  /** Timer seam for the chord timeout; tests drive it without real waits. */
  clearTimeout?: (handle: number) => void;
  setTimeout?: (callback: () => void, ms: number) => number;
  /** Throw on a prefix conflict (development); otherwise report it and keep going. */
  strict?: boolean;
}

export interface Keymap {
  /** Every registered binding with its `when()` evaluated now — the source for `?`. */
  describe(): KeyBindingDescription[];
  handleKeyDown(event: KeyboardEvent): void;
  /** The keys typed so far of an unfinished chord (`"g"`), or `""`. */
  pending(): string;
  pushScope(scope: KeymapScope): () => void;
  register(scope: KeymapScope, bindings: readonly KeyBinding[]): () => void;
  subscribe(listener: () => void): () => void;
}

interface Resolution {
  exact: Registration | undefined;
  partial: boolean;
}

export function createKeymap(options: KeymapOptions = {}): Keymap {
  const schedule = options.setTimeout ?? ((callback, ms) => window.setTimeout(callback, ms));
  const unschedule = options.clearTimeout ?? ((handle) => window.clearTimeout(handle));
  const strict = options.strict ?? false;

  const registrations: Registration[] = [];
  const scopes: KeymapScope[] = [];
  const listeners = new Set<() => void>();
  let pending: KeyPress[] = [];
  let pendingTimer: number | undefined;
  let pendingKeys = "";

  const notify = () => {
    pendingKeys = pending.map((press) => press.key).join(" ");
    for (const listener of listeners) {
      listener();
    }
  };

  const clearPending = () => {
    if (pendingTimer !== undefined) {
      unschedule(pendingTimer);
      pendingTimer = undefined;
    }
    if (pending.length > 0) {
      pending = [];
      notify();
    }
  };

  const reportConflict = (message: string) => {
    if (strict) {
      throw new Error(message);
    }
    console.error(message);
  };

  const checkPrefixConflicts = (incoming: readonly Registration[]) => {
    const existing = [...registrations, ...incoming];
    for (const candidate of incoming) {
      for (const other of existing) {
        if (other === candidate) {
          continue;
        }
        for (const chord of candidate.chords) {
          for (const otherChord of other.chords) {
            if (isStrictPrefix(chord, otherChord) || isStrictPrefix(otherChord, chord)) {
              reportConflict(
                `Keymap: "${candidate.binding.id}" (${candidate.scope}) and "${other.binding.id}" (${other.scope}) conflict — one key sequence is a prefix of the other`
              );
            }
          }
        }
      }
    }
  };

  const resolve = (presses: readonly KeyPress[], editable: boolean): Resolution => {
    // A modal owns the keyboard: nothing beneath it fires, however the scopes are ordered.
    const stack: KeymapScope[] = scopes.includes(DIALOG_SCOPE)
      ? [DIALOG_SCOPE]
      : ["global", ...scopes];
    for (let depth = stack.length - 1; depth >= 0; depth -= 1) {
      const scope = stack[depth];
      let exact: Registration | undefined;
      let partial = false;
      for (const registration of registrations) {
        const { binding } = registration;
        if (registration.scope !== scope || (editable && binding.inEditable !== true)) {
          continue;
        }
        for (const chord of registration.chords) {
          if (
            chord.length < presses.length ||
            !presses.every((press, index) => matchesCombo(chord[index] as KeyCombo, press))
          ) {
            continue;
          }
          if (chord.length > presses.length) {
            partial = true;
          } else if (exact === undefined && binding.when?.() !== false) {
            exact = registration;
          }
        }
      }
      if (exact !== undefined || partial) {
        return { exact, partial };
      }
    }
    return { exact: undefined, partial: false };
  };

  return {
    describe() {
      return registrations.map(({ binding, scope }) => ({
        enabled: binding.when?.() !== false,
        id: binding.id,
        keys: keysOf(binding),
        label: binding.label,
        scope,
      }));
    },
    handleKeyDown(event) {
      if (event.defaultPrevented || event.isComposing || MODIFIER_KEYS[event.key] === true) {
        return;
      }
      const editable = isEditableTarget(event.target);
      const press: KeyPress = {
        altKey: event.altKey,
        ctrlKey: event.ctrlKey,
        key: event.key,
        metaKey: event.metaKey,
        shiftKey: event.shiftKey,
      };
      let presses = [...pending, press];
      let resolution = resolve(presses, editable);
      if (resolution.exact === undefined && !resolution.partial && pending.length > 0) {
        // A key that does not continue the chord is read afresh on its own.
        presses = [press];
        resolution = resolve(presses, editable);
      }
      if (resolution.exact !== undefined) {
        event.preventDefault();
        clearPending();
        resolution.exact.binding.run(event);
        return;
      }
      if (resolution.partial) {
        event.preventDefault();
        if (pendingTimer !== undefined) {
          unschedule(pendingTimer);
        }
        pending = presses;
        pendingTimer = schedule(() => {
          pendingTimer = undefined;
          clearPending();
        }, CHORD_TIMEOUT_MS);
        notify();
        return;
      }
      clearPending();
    },
    pending() {
      return pendingKeys;
    },
    pushScope(scope) {
      scopes.push(scope);
      return () => {
        const index = scopes.lastIndexOf(scope);
        if (index !== -1) {
          scopes.splice(index, 1);
        }
      };
    },
    register(scope, bindings) {
      const incoming = bindings.map((binding) => ({
        binding,
        chords: keysOf(binding).map((keys) => keys.trim().split(/\s+/).map(parseCombo)),
        scope,
      }));
      checkPrefixConflicts(incoming);
      registrations.push(...incoming);
      return () => {
        for (const registration of incoming) {
          const index = registrations.indexOf(registration);
          if (index !== -1) {
            registrations.splice(index, 1);
          }
        }
      };
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** The application's registry; `KeymapProvider` binds it to `window`. Unit tests build their own. */
export const appKeymap = createKeymap({ strict: import.meta.env.DEV });

/** Registers `bindings` under `scope` for the component's lifetime; closures stay current. */
export function useKeymap(scope: KeymapScope, bindings: readonly KeyBinding[]): void {
  const latest = useRef(bindings);
  latest.current = bindings;
  const signature = bindings.map((binding) => `${binding.id}=${keysOf(binding).join(",")}`).join();

  // biome-ignore lint/correctness/useExhaustiveDependencies: `signature` stands in for `bindings`, whose closures are read through `latest`
  useLayoutEffect(() => {
    const proxies = latest.current.map((binding, index) => ({
      ...binding,
      run: (event: KeyboardEvent) => latest.current[index]?.run(event),
      when: () => latest.current[index]?.when?.() !== false,
    }));
    return appKeymap.register(scope, proxies);
  }, [scope, signature]);
}

/** Pushes `scope` onto the stack while mounted; `null` pushes nothing (a closed dialog). */
export function useKeymapScope(scope: KeymapScope | null): void {
  useLayoutEffect(() => (scope === null ? undefined : appKeymap.pushScope(scope)), [scope]);
}

/** The keys typed so far of an unfinished chord (`"g"`), or `""`. */
export function usePendingChord(): string {
  return useSyncExternalStore(appKeymap.subscribe, appKeymap.pending);
}
