import { describe, expect, it } from "bun:test";
import { parseExportedSymbols } from "../parser";

describe("parseExportedSymbols", () => {
  it("returns an empty array when a file has no exports", () => {
    expect(parseExportedSymbols("const x = 1;\nfunction y() {}\n")).toEqual([]);
  });

  it("extracts exported symbols across supported declaration kinds", () => {
    const source = `
export function namedFunction(value: string) {
  return value;
}
export async function namedAsyncFunction() {
  return Promise.resolve();
}
export class ExampleClass {
  value = 1;
}
export type ExampleType = { name: string };
export interface ExampleInterface {
  id: string;
}
export const EXAMPLE_CONST = 123;
export let exampleLet = "hello";
export default function DefaultFunction() {
  return null;
}
export default 42;
export { ReExported, AlsoReExported as Alias } from "./other";
export * from "./everything";
`;

    expect(parseExportedSymbols(source)).toEqual([
      {
        name: "namedFunction",
        kind: "function",
        signature: "export function namedFunction(value: string) {",
      },
      {
        name: "namedAsyncFunction",
        kind: "function",
        signature: "export async function namedAsyncFunction() {",
      },
      {
        name: "ExampleClass",
        kind: "class",
        signature: "export class ExampleClass {",
      },
      {
        name: "ExampleType",
        kind: "type",
        signature: "export type ExampleType = { name: string };",
      },
      {
        name: "ExampleInterface",
        kind: "interface",
        signature: "export interface ExampleInterface {",
      },
      {
        name: "EXAMPLE_CONST",
        kind: "const",
        signature: "export const EXAMPLE_CONST = 123;",
      },
      {
        name: "exampleLet",
        kind: "let",
        signature: 'export let exampleLet = "hello";',
      },
      {
        name: "default",
        kind: "default",
        signature: "export default function DefaultFunction() {",
      },
      {
        name: "default",
        kind: "default",
        signature: "export default 42;",
      },
      {
        name: "ReExported",
        kind: "reexport",
        signature: 'export { ReExported, AlsoReExported as Alias } from "./other";',
      },
      {
        name: "AlsoReExported",
        kind: "reexport",
        signature: 'export { ReExported, AlsoReExported as Alias } from "./other";',
      },
      {
        name: "*",
        kind: "reexport",
        signature: 'export * from "./everything";',
      },
    ]);
  });

  it("uses the first line for signatures and truncates to 200 characters", () => {
    const longName = "A".repeat(240);
    const source = `export const ${longName} = 1;\nexport interface MultiLine {\n  value: string;\n}`;
    const parsed = parseExportedSymbols(source);

    expect(parsed[0]?.name).toBe(longName);
    expect(parsed[0]?.signature.length).toBe(200);
    expect(parsed[1]).toEqual({
      name: "MultiLine",
      kind: "interface",
      signature: "export interface MultiLine {",
    });
  });
});
