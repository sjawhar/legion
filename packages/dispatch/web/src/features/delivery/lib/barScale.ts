// The vertical scale `Sparkbars` and `SpreadBars` share, from the prototype's own two copies of it
// (web/src/components/Sparkbars.tsx and SpreadBars.tsx). Pure.

/** A bar chart's y coordinate for a value, measured down from the top of a chart `height` tall,
 *  on a scale whose top reaches the floor, a quarter above the target (so its line never sits at
 *  the top edge) and the tallest value. */
export function barScale(
  floor: number,
  target: number | undefined,
  values: readonly (number | null)[],
  height: number
): (value: number) => number {
  const top = Math.max(
    floor,
    target === undefined ? 0 : target * 1.25,
    ...values.map((v) => v ?? 0)
  );
  return (value) => height - (value / top) * height;
}
