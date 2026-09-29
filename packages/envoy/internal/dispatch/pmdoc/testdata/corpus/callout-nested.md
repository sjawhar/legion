::::callout{#callout-nested-1 kind="note" title="Outer"}
Outer text.

:::callout{#callout-nested-2 kind="warning" title="Inner"}
Inner text.
:::
::::

::::callout{#callout-nested-3 kind="note" title="Code"}
```text
a
:::
b
```
::::

After.

A callout whose opening line is indented, holding code:

  ::::callout{#ind kind="note" title="T"}
      b
  ::::

And one holding a callout its closing line, indented past the outer opener's column, closes:

  ::::callout{#ino kind="note" title="T"}
  :::callout{#inn kind="note" title="T"}
  a
     :::
  ::::
