-- 0069_people_clear_plain_refresh_tokens.up.sql
-- Dispatch seals each refresh token it keeps in people (AES-256-GCM under a key derived from
-- DISPATCH_SIGNING_KEY; store.refreshTokenSeal), and every sealed value begins `v1:`. A token
-- stored in plain text before that cannot be sealed in SQL, so this forgets it with the
-- confirmation people's check pairs it with, keeping the person: each person who signed in before
-- this release signs in again once. A sealed value is left as it is, so applying this again changes
-- nothing.
update people set refresh_token = null, confirmed_at = null
where refresh_token is not null and refresh_token not like 'v1:%';
