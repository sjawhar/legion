# Rollout spec

The service ships behind a flag.

## Writing a decision block

An example of the block an architect writes, kept as text:

```markdown
:::ask{urgency="high" multiple="false"}
Which region goes first?

- East: The quiet one.
- West: The busy one.
:::
```

:::ask{#rollout urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-05T14:30:00Z" selected="[&#x22;Two&#x22;]"}
How many regions take the first wave?

- Two: The two with the fewest tenants.
- Five: Half of them.
:::

The platform team asked for a rollback path.
