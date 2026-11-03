> - ```
>   Text[^f] and[^g].
>   - > ~~~
>   > code
>     > ~~~
>     > code
>     y
>     ===
>   
>     [^g]:
          indented
>         ```
>       code
>     ```
>     
> [^f]:

> [^g]::::callout{}
    :::callout{}
>     x
> 
> :::

>     :::callout{kind="note" title="T"}
>     note
>     Text[^f] and[^g].
>     
>     note
>     > ***
>     > ***
>     > 
>     :::
>     x
>     y
>     ===
>     


:::callout{kind="note" title="T"}
> 1.     indented
   1. y
>       ===

>       ~~~
>       code
>       
      ```
>       code
>       ```
>       
> 

> [^g]:
>     
>     x
> ```
y
===

:::
