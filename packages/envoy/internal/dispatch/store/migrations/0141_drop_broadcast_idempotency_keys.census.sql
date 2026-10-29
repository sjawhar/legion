-- The drop deletes the keys of broadcasts sent before 0137's release. No running server reads them:
-- every send since keys through request_keys, and the send route no longer takes the body key these
-- rows recorded. No stored value anything still reads is lost, so nothing here is one to inspect
-- before the deploy.
select 0;
