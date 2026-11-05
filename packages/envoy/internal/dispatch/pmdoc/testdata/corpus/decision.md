# Decision nodes

::::decision{#one-source title="Keep one source" shaped="dispatch://CORE-1/spec#b-rule@v1" from_ask="dispatch://CORE-1/ask/a1" decided_by="alice"}
Use the original row, not a copied answer.

:::callout{#why kind="note" title="Why"}
Relations stay on the decision node when it moves.
:::

Unsure: a source that cannot retain identity would change this choice.
::::

:::decision{#old-file title="Keep a separate file" replaced="true"}
The file has been replaced by native nodes.
:::
