-- The rows 0070's check would refuse. session_id does not exist before 0070 and is null in every
-- row once it is added, so the check reduces to its first branch, project_key is not null, here
-- negated; 0009's not null holds it today, so this answers 0 on any database 0009 built. DROP
-- EXPRESSION keeps every ref_key and the trigger touches no existing row, so nothing is rewritten.
select count(*) from artifacts where project_key is null;
