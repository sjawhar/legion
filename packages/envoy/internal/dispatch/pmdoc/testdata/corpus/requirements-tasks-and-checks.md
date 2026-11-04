:::requirement{#r1}
The issue page lists every open ask, the most urgent first.
:::

:::requirement{#r2}
An answered ask leaves the list.
:::

:::plan_task{#t1 serves="dispatch://CORE-1/artifact/plan-md#b-r1"}
Read the issue's open asks and draw them.
:::

:::plan_task{#t2 serves="dispatch://CORE-1/artifact/plan-md#b-r1 dispatch://CORE-1/artifact/plan-md#b-r2" needs="dispatch://CORE-1/artifact/plan-md#b-t1"}
Sort the drawn asks by urgency, then drop the answered ones.
:::

:::check{#k1 serves="dispatch://CORE-1/artifact/plan-md#b-r1" surface="browser" run="bun test web/src/features/issue" when="before_merge" negative_control="Draw the asks unsorted and watch this fail."}
The issue page draws the open asks, the most urgent first.
:::

:::check{#k2 serves="dispatch://CORE-1/artifact/plan-md#b-r2" when="after_merge"}
No answered ask is drawn, written as a claim with nothing to run.
:::
