-- One nullable column on projects; every project's default_assignee is null, which the check
-- does not test. No row is refused or rewritten.
select 0;
