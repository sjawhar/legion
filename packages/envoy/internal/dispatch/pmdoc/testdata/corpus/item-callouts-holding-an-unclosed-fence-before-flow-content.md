Callouts in list items, each holding an unclosed fence that ends in a blank line, before flow content the item does not hold.

- x

  :::callout{#uf1 kind="note" title="T"}
  ~~~
  code

after

- x
  :::callout{#uf2 kind="note" title="T"}
  ```
  code

# Heading

1. x
   :::callout{#uf3 kind="note" title="T"}
   ~~~
   code
   more


***

-   x
    :::callout{#uf4 kind="note" title="T"}
    ```
    code

:::callout{#uf5 kind="note" title="T"}
z
:::

- x
  ::::callout{#uf6 kind="note" title="O"}
  :::callout{#uf7 kind="note" title="T"}
  ~~~
  code

~~~
z
~~~

- x
  :::callout{#uf8 kind="note" title="T"}
  ````js
  code

| a |
| - |
| b |

- a
  - x

    :::callout{#uf9 kind="note" title="T"}
    ~~~
    code

  after

- a
  - x
    :::callout{#uf10 kind="note" title="T"}
    ```
    code


after
