-- 0065 changes comments.created_at's default and nothing else: a default applies only to rows
-- inserted after it, so no existing row is refused or rewritten.
select 0;
