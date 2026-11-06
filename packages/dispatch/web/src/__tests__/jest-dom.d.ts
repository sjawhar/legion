/**
 * The jest-dom matchers the preload registers (`setup.ts`), declared for `bun:test`'s `expect`.
 *
 * `@testing-library/jest-dom` ships its declarations for jest's and vitest's `expect`, neither of
 * which `bun:test` is, so the matchers it hands `expect.extend` are typed here against Bun's own
 * `Matchers` and `AsymmetricMatchers` interfaces.
 */

import type { TestingLibraryMatchers } from "@testing-library/jest-dom/matchers";

declare module "bun:test" {
  interface Matchers<T> extends TestingLibraryMatchers<unknown, T> {}
  interface AsymmetricMatchers extends TestingLibraryMatchers<unknown, void> {}
}
