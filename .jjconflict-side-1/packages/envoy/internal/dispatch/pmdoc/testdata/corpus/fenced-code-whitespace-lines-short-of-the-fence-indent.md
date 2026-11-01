Fenced code whose opening fence stands past its container's content drops a whitespace-only line holding fewer columns than that past what its containers eat, and keeps what a line of as many columns or more holds past them; a fence at its container's content keeps the whole line.

A line short of the fence's indentation, the code's last:

  ~~~
 
  ~~~

Before a line of code:

  ~~~
 
x
  ~~~

In a callout:

:::callout{}
  ~~~
 
  x
  ~~~
:::

In a quote:

>   ~~~
>  
>   x
>   ~~~

A fence at the line's start keeps the line:

~~~
 
x
~~~

A line past the fence's indentation:

  ~~~
   
x
  ~~~

A fence at the quote's content:

> ~~~
>  
> x
> ~~~

A fence at the item's content:

-   ~~~
   
    x
    ~~~

A fence past a footnote definition's label:

x[^f]

[^f]:   ~~~
     
      x
      ~~~
