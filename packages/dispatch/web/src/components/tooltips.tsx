import { createContext, type ReactNode, useContext } from "react";

/**
 * Whether the surface a control is drawn on offers pointer tooltips.
 *
 * Every shared control carries its `title` as it always has, because a tooltip is a real
 * affordance on a surface a reader points at. One surface does not: the issue page
 * (dispatch://INFRA-1196 R2, R21), where a fact a pointer alone can reach is a fact a reader on
 * a phone or on the keyboard does not have, so the page draws what it means and carries no
 * `title` at all. The page says so once, around its own column and around its panel, and every
 * control inside answers by leaving its `title` off.
 *
 * It is a context rather than a prop because the panel holds the margin's own cards, and the
 * margin is mounted beside the router, not by the page: the cards reach this through the fill
 * the page wraps them in (`features/margin/margin-slot.tsx`) and through nothing else.
 *
 * A control that holds meaning no visible text carries must draw that meaning here rather than
 * dropping it; `Timestamp`'s `exact` and `ClaimChip` are how the issue page does it.
 */
const TooltipsContext = createContext(true);

/** Everything inside draws no `title`. */
export function NoTooltips({ children }: { children: ReactNode }): ReactNode {
  return <TooltipsContext.Provider value={false}>{children}</TooltipsContext.Provider>;
}

/** Whether this control may carry a `title`. True everywhere but inside `NoTooltips`. */
export function useTooltips(): boolean {
  return useContext(TooltipsContext);
}
