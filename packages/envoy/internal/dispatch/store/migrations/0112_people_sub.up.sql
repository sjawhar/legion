-- The Google account (`sub`) that last signed in to each address with Google for a Workspace domain
-- (DISPATCH_SIGNIN_DOMAIN). Google never reassigns a subject, but a Workspace admin can give a
-- deleted account's address to a new account, and a person is keyed by address: a sign-in under
-- another subject than the row holds ends every session of the address before it records its own
-- (store.PgPeopleStore.SignInSubject), and the hourly confirmation ends them when the refreshed token names
-- another subject (identity.Membership). Null for an address no such sign-in recorded: a person
-- signed in through a Cognito sign-in pool, which neither reads nor writes it, recorded any other
-- way, or signed in before this column; no row is written here.
alter table people add column sub text;
