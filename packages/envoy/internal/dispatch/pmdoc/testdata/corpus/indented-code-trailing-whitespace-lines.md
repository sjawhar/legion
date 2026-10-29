Indented code keeps the whitespace-only lines after it that hold four columns or more past where its container's content starts, and any shorter blank lines between them; a list item measures from its content, a footnote definition from its own start.

Top level, before a paragraph:

          x
        
       
After.

Top level, one line of exactly four columns, then a blank line:

    code
    

After.

Two lines of exactly four columns:

    code
    
    
After.

A tab past the code's columns:

    code
		
After.

>           x
>        
>       
> In the quote.

- a

          x
        
       
  In the item.

Next.

1. a

           x
         

   In the item.

Next.

- a
  - b

            x
              

Next.

- a

  >     x
  >       

Next.

:::callout{kind="note" title="T"}
    code
      
:::

Footnote[^q].

[^q]: a

          x
        
       
    y

Code right after a list or a quote keeps none, as main reads it:

100. a

    x
          
After.

> # h
    x
      
End.
