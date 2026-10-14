# Rollout plan

The service ships behind a flag.

Two regions take the first wave, and the rest follow a week later.

## Decisions

:::ask{#wave-size urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-01T09:00:00Z" selected="[&#x22;Two&#x22;]" answer="Two regions first."}
How many regions take the first wave?

- Two: The two with the fewest tenants.
- Five: Half of them.
:::

> The platform team asked for a rollback path.
>
> :::ask{#rollback urgency="high" multiple="false" state="open"}
> Is a feature flag enough as a rollback path?
>
> - Yes: Flip the flag.
> - No: Keep the old deployment warm.
> :::

- Announce the dates.
- Pick the regions.

  :::ask{#regions urgency="low" multiple="false" state="open"}
  Which regions go first?
  :::

::::callout{#budget-note kind="note" title="Open question"}
The budget is not settled.

:::ask{#budget urgency="med" multiple="false" state="open"}
Who pays for the second wave?
:::
::::

The last paragraph closes the plan.
