Lists inside an unclosed typed block in a quote, after the blank lines that end the block: the list the next item belongs to, and the whitespace those lines hold.

> :::callout{#qi1 kind="note" title="T"}
> - x
>   - y
>
> - z

Next.

> - a
>   :::callout{#qi2 kind="note" title="T"}
>   - x
>     - y
>
>   - z

Next.

> - a
>   :::callout{#qi3 kind="note" title="T"}
>   - x
>	
>	

Next.

> 1. a
>    :::callout{#qi4 kind="note" title="T"}
>    - x
>	
>	
