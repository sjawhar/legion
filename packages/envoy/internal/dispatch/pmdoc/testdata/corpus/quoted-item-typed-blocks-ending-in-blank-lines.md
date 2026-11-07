Blank lines ending a typed block in a quoted list item, which the browser editor's parser reads as
the lines of the list the block ends with and not as spacing the item.

Next.

> - a
>   :::callout{#c1 kind="note" title="T"}
>   - x
>
>   :::

Next.

> - a
>   :::callout{#c2 kind="note" title="T"}
>   - x
>   
>   :::

Next.

> - a
>   :::callout{#c3 kind="note" title="T"}
>   - x
>
>
>   :::

Next.

> - a
>   :::callout{#c4 kind="note" title="T"}
>   - x
>     - y
>
>   :::

Next.

> - a
>   :::callout{#c5 kind="note" title="T"}
>   - x
>         code
>
>   :::

Next.

> > - a
> >   :::callout{#c6 kind="note" title="T"}
> >   - x
> >
> >   :::

Next.

> 1. a
>    ::::callout{#c7 kind="note" title="T"}
>    :::callout{#c8 kind="note" title="T"}
>    - x
>
>
>    ::::

Next.

> - a
>   :::callout{#c9 kind="note" title="T"}
>   - x
>
