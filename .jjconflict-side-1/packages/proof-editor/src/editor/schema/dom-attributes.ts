import type { DOMOutputSpec } from "@milkdown/kit/prose/model";

function isAttrsObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value) && !("nodeType" in value);
}

/** Adds attributes to a DOMOutputSpec without disturbing its content hole. */
export function withDomAttributes(
  spec: DOMOutputSpec,
  attributes: Record<string, string>,
): DOMOutputSpec {
  if (Array.isArray(spec)) {
    const [tag, second, ...rest] = spec as unknown[];
    if (isAttrsObject(second)) {
      return [tag, { ...second, ...attributes }, ...rest] as unknown as DOMOutputSpec;
    }
    return [tag, attributes, second, ...rest].filter(
      (part) => part !== undefined,
    ) as unknown as DOMOutputSpec;
  }
  if (typeof spec === "object" && spec !== null && "dom" in spec) {
    for (const [name, value] of Object.entries(attributes)) {
      (spec.dom as Element).setAttribute?.(name, value);
    }
    return spec;
  }
  if (typeof spec === "object" && spec !== null && "setAttribute" in spec) {
    for (const [name, value] of Object.entries(attributes)) {
      (spec as Element).setAttribute(name, value);
    }
  }
  return spec;
}
