x `a
 b` y

x `a
	b` y

x `a
  ` y

x ` 
foo
 ` y

A tab a container's marker takes part of, before a code span's later line:

> x ``
>	 ``

Next.

> x ``
>	    ``

Next.

- x ``
	  ``

Next.

> x `` a
>	``

Next.

> x ``
>	``

Next.

> x ``
> 	 ``

Next.

- x `` a
	``

Next.

> x `` a
>	 b``

Next.

Later lines reaching a list item's or a definition's columns:

- x ``
    a``

Next.

- - x ``
      a``

Next.

- > x ``
    a``

Next.

x[^n]

[^n]: x ``
        a``

Next.

- x ``
   a``

Next.

> - x ``
>     a``
