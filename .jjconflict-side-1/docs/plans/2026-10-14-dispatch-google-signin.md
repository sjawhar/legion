# Dispatch sign-in with Google Workspace: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Dispatch signs people in with Google, admitting anyone whose Google account belongs to one
configured Workspace domain and whose email Google has verified, with no Cognito. A deployment
without AWS can then run Dispatch. A deployment that sets the four Cognito sign-in settings today
sees nothing change: the same cookie, seal, email derivation, AuthURL, scopes, revoke, refusal
pages, log lines and hourly confirmation.

**Architecture:** one new setting, `DISPATCH_SIGNIN_DOMAIN`, takes the place of
`DISPATCH_SIGNIN_GROUP` beside the issuer, client id and client secret: exactly one of the two is
set, and a domain needs Google's issuer. With the domain set, the code flow asks Google for offline
access, and a person is the lowercase `email` of a token whose `hd` equals the domain and whose
`email_verified` is true, decoded in the code flow's own session rather than in the verifier other
token kinds share. The hourly confirmation runs the same rule on the refreshed token. In domain mode
the person's Google `sub` is stored, and a sign-in under another `sub` for the same address ends
the address's existing sessions. Everything else downstream of the person (the session cookie, the
sealed refresh token, the email-keyed tables, personal tokens) is unchanged.

**Tech stack:** Go (`packages/envoy`), go-oidc v3.21.0 (`go.mod:16`), the existing `oidctest` fake
issuer.

---

## Facts this plan rests on

From a read of the code at `1a2da912` and Google's OpenID Connect and OAuth pages (opened
2026-10-14); file references are under `packages/envoy/` unless they start with a top-level
directory.

- Sign-in is the OIDC code flow in `internal/oidc/codeflow.go` (`AuthURL` 91-95, `Exchange`
  100-118). `internal/oidc/verifier.go:68-87,120-142` reads `cognito:username`, `cognito:groups` and
  `identities` for every token it checks, which includes pods' service-account tokens
  (`internal/dispatch/api/server.go:85,626-627`) and the Envoy listener's (`verifier.go:1-4`).
  `internal/dispatch/identity/person.go:30-52` names the person from the username and checks the
  group. Groups gate sign-in and nothing else (`server.go:591-619,707-719`; `.Groups` is read only
  at `person.go:35`).
- `Exchange` requires a refresh token (`codeflow.go:114-116`). Google returns one only for
  `access_type=offline`, and only on the first exchange unless `prompt=consent`. Logout forgets the
  token without revoking it at Google (`internal/dispatch/routes/router.go:335-352`).
- `identity.Membership` (`internal/dispatch/identity/membership.go:49-104`) refreshes once the last
  confirmation is an hour old, using the refresh token the address's row holds at that moment, runs
  the person rule on the refreshed token, and ends every session of the person on any refusal, any
  refresh error (network failures included, inside the 15 s `refreshTimeout`, `:79-82`) or a
  changed email. `Refresh` requires an `id_token` in the response (`codeflow.go:165-170`). The
  binary has no test clock: `requestIdentityFor` builds `Membership` without `Now`
  (`cmd/dispatch/signin.go:117`), so it uses `time.Now` (`membership.go:110-114`).
- The person key is the lowercase email. A sign-in replaces the address's refresh token and
  `confirmed_at` (`internal/dispatch/store/people.go:64-68`), and `EnsureSession` keeps the existing
  session generation (`store/sessions.go:25-49`), so two people who hold one address in turn share
  the generation their cookies are checked against (`identity/identity.go:42-53`).
- Google's ID token: `iss` is `https://accounts.google.com` or `accounts.google.com` (go-oidc accepts
  both when the configured issuer is exactly `https://accounts.google.com`, `verify.go:237-246`); `hd`
  is present only for a Workspace account and is the claim to check (the `email`'s domain must not
  be used); `email_verified` may be a bool or the string `"true"`; `sub` is never reassigned.
- The sign-in settings and their refusals: `cmd/dispatch/signin.go:21-38` (`signInSettingNames`),
  `:43-59` (`checkSignInSettings`, which returns nil for a deployment with all four), `:55-56`
  (the partial-set text), `:67` (`requireCookieSignIn`); header identity refuses them by looping over
  `signInSettings` (`:45-54`), dev sign-in by checking `SignInIssuer` alone (`cmd/dispatch/main.go:622-623`).
  Every variable is a row of the settings table in `cmd/dispatch/settings.go`
  (`TestNoReaderBypassesTheSettingsTable`, `TestEverySettingReachesItsReader`,
  `settings_test.go:1149-1153`), and the configuration reference is generated from it
  (`docs/site/generators/dispatch-config.ts:2-3`).
- The compose deployment hands the container an explicit environment list, which names the four
  sign-in settings (`deploy/compose/dispatch.compose.yml:40-43`).
- The fake issuer `oidctest.Issuer` mints any claims a test gives it, and serves `UpdateGrants` and
  `RevokeGrants` (`internal/oidc/oidctest/issuer.go:259-283,416-484`).

## Decisions

- **D1. Mode selection and refusals.** `DISPATCH_SIGNIN_DOMAIN` is a new row. With
  `DISPATCH_SIGNIN_ISSUER`, `_CLIENT_ID` and `_CLIENT_SECRET`, exactly one of
  `DISPATCH_SIGNIN_GROUP` and `DISPATCH_SIGNIN_DOMAIN` is set. The domain counts as a sign-in
  setting everywhere the four do: header identity refuses it, dev sign-in refuses it, and a domain
  without the other three is a partial set. A domain needs the issuer exactly
  `https://accounts.google.com` (only Google's tokens make `hd` mean Workspace membership), and a
  group with that issuer refuses (a Google token carries no `cognito:groups`, so it would admit
  nobody). A domain must be a lowercase DNS name. Byte-identical means what a deployment with all
  four Cognito settings sees; it reaches none of the texts below, since `checkSignInSettings`
  returns nil for it. The new texts follow the form of `signin.go:55-56,67`:
  - neither group nor domain beside the three: `Google sign-in needs DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_CLIENT_SECRET and one of DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN: <set ones> set, DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN missing`;
  - a partial set: the same lead, then `: <set ones> set, <missing ones> missing`, where the pair is spelled `DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN` as one missing item;
  - both: `DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN are both set; set one: a Cognito pool's group, or a Google Workspace domain`;
  - a domain with another issuer: `DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER https://accounts.google.com, since only Google's tokens carry hd; DISPATCH_SIGNIN_ISSUER is <issuer>`;
  - a group with Google's issuer: `DISPATCH_SIGNIN_GROUP is a Cognito pool's group, and a token from https://accounts.google.com carries none; set DISPATCH_SIGNIN_DOMAIN instead`;
  - a malformed domain: `DISPATCH_SIGNIN_DOMAIN must be a lowercase domain name such as example.com; got a value of <n> characters` (the value itself is never echoed, so a secret pasted into the wrong variable stays out of the boot log);
  - `requireCookieSignIn` and `signInUnconfigured`'s 503 (`routes/router.go:380`): their lists name `one of DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN` where they name the group today;
  - header identity names `DISPATCH_SIGNIN_DOMAIN` when it is the setting it finds. Dev sign-in's fence checks only the issuer and runs after `checkSignInSettings` (`main.go:541`, then `:622-623`), so a domain beside dev sign-in is refused first by the checks below, and the fence refuses only a complete domain set.

  The checks run in this order and the first that fails is the refusal: both set; a partial set
  (the neither case included); a domain under another issuer; a malformed domain; a group under
  Google's issuer. The assertions whose expected strings change: `cmd/dispatch/main_test.go:81-84`,
  `settings_test.go:550,601`; the header loop at `main_test.go:108-118` gains the domain, and
  `TestDevSignInRefusesTheSignInSettings` (`main_test.go:122-130`) gains a case built from
  Google's issuer, the client id, the secret and a domain, with the group empty.
- **D2. The person key stays the lowercase email; in domain mode `sub` is stored and checked.** A
  migration adds a nullable `people.sub`. `sub` crosses `auth.PeopleStore` and `PersonMembership`
  (`auth/people.go:14-32`), whose signatures gain it, as do the two test stores that implement them
  (`routes/router_test.go:66-110`, `identity/identity_test.go:61-102`). In domain mode, a sign-in's
  one transaction reads the row's `sub`, calls `RevokeSessions` for the address when it differs from
  a non-null stored one, and only then writes the new `sub` with the refresh token, so a failed
  revoke leaves the old `sub` in place and the retry revokes again; it logs the revoke at WARN with
  the address. The hourly confirmation ends the address's sessions when the refreshed token's `sub`
  differs from the stored one. Group mode never reads or writes `sub`. Without this, the old holder of a reassigned
  address keeps their cookies until they expire (30 days, `auth/session.go:21`) once the new holder
  signs in first, because the confirmation then refreshes with the new holder's token and passes.
  The same residual stays in group mode, where the pool user follows the address: closing it there
  needs another key and is out of this plan's scope.
- **D3. Code flow.** `NewCodeFlow` and `DiscoverCodeFlow` (`codeflow.go:49,83`) take a hosted
  domain, empty in group mode. Only when it is set does `AuthURL` add `access_type=offline`,
  `prompt=consent` and `hd=<domain>`. The Cognito AuthURL (pinned by `codeflow_test.go:77-93`),
  scopes and Basic-auth revoke are unchanged. Callers: `cmd/dispatch/signin.go:77`,
  `codeflow_test.go:28,215,231,240`, `routes/router_test.go:140`, `identity/membership_test.go:44`,
  `store/people_test.go:104`, `api/events_identity_test.go:238`.
- **D4. Claims.** `email`, `email_verified` (bool, or the string `"true"`; any other value or shape
  is false, never an error) and `hd` are decoded only in domain mode, in the code flow's session
  from the verified ID token, beside `sub`. `Verify` is untouched, so Cognito sign-in, pods' tokens
  and the listener decode exactly what they do today.
- **D5. Person rule in domain mode:** `hd` equals the domain exactly; `email_verified` is true;
  `email` is present and matches the existing email shape. Each refusal has its own reason
  (`not in the domain`, `email not verified`, `no email`) on the refusal page. The identity
  package's `SignInRule` (group or domain, exactly one) replaces `Membership.Group`,
  `AppContextOptions.SignInGroup` and `Person`'s group argument; the values change at
  `router_test.go:145,152`, `membership_test.go:63-67`, `events_identity_test.go:271`,
  `cmd/dispatch/main.go:792`, `signin.go:117` and `router.go:129-130`, with every existing
  assertion unchanged.
- **D6. Logs.** Boot logs the domain in domain mode where it logs the group (`main.go:243`); a
  refusal logs the email and `hd` where it logs the Cognito username (`router.go:302`). Group mode's
  lines are unchanged (`router_test.go:266-290`).
- **D7. Refresh errors.** Membership already ends sessions on any refresh error; this plan keeps
  that and says in the docs that an outage at the issuer signs people out as their hour lapses.
- **D8. Public text.** No Workspace domain, client id, secret, email or person's name goes in a
  commit, the PR body or a comment; write `example.com`, and name the client's creator by role (the
  organization's Workspace admin).

## Tasks

### Task 1: the setting and boot checks

**Files:** `cmd/dispatch/settings.go`, `signin.go`, `main.go`, `main_test.go`, `settings_test.go`.

- [x] Test first: a domain with the three and Google's issuer passes; each D1 refusal, with its
  text; the domain beside header identity, beside dev sign-in, and alone; the changed assertions
  listed in D1; a four-setting Cognito deployment passes as today. `TestEverySettingReachesItsReader`
  gets a domain case built on the group case at `settings_test.go:595-650`: resolve under
  `https://accounts.google.com` with `DISPATCH_SIGNIN_GROUP` empty (`bootEnvironment`,
  `settings_test.go:246`, takes `signInEnvironment`, whose group is `main_test.go:42`) and assert
  the boot field, then point `boot.SignInIssuer` at the fake issuer's URL before the router
  subtest, so no test calls Google; plus a refusal, and a router sign-in with Google-shaped claims
  and an aged confirmation (a domain sign-in first passes with Task 2; the subtest landed in Task
  3's commit). Run, see them fail.
- [x] Add the row; update the issuer row's "all four … or none" (`settings.go:50`), the group row's
  Required column (`:55`) and the refusal sentence at `:112`; implement the checks; thread the
  `SignInRule` to the router and membership.
- [x] Run `go test -v ./cmd/dispatch/` with `DISPATCH_TEST_DATABASE_URL` set (`scripts/dev-postgres.sh`;
  `storetest.Open` skips without it, `storetest.go:51-61`), and confirm the new cases ran, not skipped;
  pass.

### Task 2: the code flow, the claims, the person rule

**Files:** `internal/oidc/codeflow.go`, `internal/dispatch/identity/person.go`, their tests, and the
D3 and D5 callers.

- [x] Test first: in domain mode the session decodes `email`, `hd`, `sub` and `email_verified` as
  `true`, `"true"`, `"false"`, a number, null and an object (the last four false, none an error);
  the domain rule admits a verified email with the right `hd` and refuses a wrong `hd`, a missing
  `hd`, an unverified email and a missing email, each with its reason; a Cognito-shaped token still
  verifies through `Verify` with its fields unchanged; in domain mode AuthURL carries
  `access_type=offline`, `prompt=consent` and `hd=<domain>`, and in group mode none of them (read
  the redirect's `Location`, as `router_test.go:961-975` does).
- [x] Implement.
- [x] Run `go test ./internal/oidc/ ./internal/dispatch/identity/`; pass.

### Task 3: sign-in, `sub`, refusal page and hourly confirmation end to end

**Files:** `internal/dispatch/routes/router.go`, `internal/dispatch/identity/membership.go`,
`internal/dispatch/store/people.go` and a new migration with its census file
(`cmd/dispatch/README.md:600-608`), their tests, `api/events_identity_test.go`.

- [x] Test first, against `oidctest.Issuer` with Google-shaped claims: a domain member signs in and
  gets a session; a non-member and an unverified email reach the refusal page with their reason, no
  session and the D6 log line; the hourly confirmation keeps a member's sessions and ends them when
  the refreshed token's `hd`, email or `sub` changes, or the refresh fails; two Google subjects share
  one address, and after the second signs in the first one's cookie answers 401 while the second's
  works; the same subject signing in again keeps its other sessions; group mode writes no `sub`;
  the Cognito tests' assertions are unchanged and pass. Run, see them fail.
- [x] Implement.
- [x] Run `go test -v ./internal/dispatch/...` and the migration census check with
  `DISPATCH_TEST_DATABASE_URL` set, and confirm the new store and router cases ran; pass.

### Task 4: docs and deployment files

**Files:** `docs/site/src/content/docs/dispatch/running-dispatch.md`, `cmd/dispatch/README.md`
(including the route table at `:781-782`), `cmd/dispatch/AGENTS.md` (Identity, and the route table
at `:373-374`), `packages/envoy/AGENTS.md:1824`, the comment at `router.go:269-272`,
`deploy/compose/dispatch.compose.yml` (`DISPATCH_SIGNIN_DOMAIN: ${DISPATCH_SIGNIN_DOMAIN:-}`),
`deploy/compose/dispatch.env.example`, `deploy/README.md:148`, `skills/dispatch/references/api.md:30-31`,
and `packages/dispatch/e2e/run-server.sh:89` if its settings need the new name.

- [x] Document the two sign-in modes, the Google client to create (a web client, the redirect URI
  `<origin>/auth/callback`, whose host must be https and end in a public suffix, or loopback http
  for local use), the domain setting, D2's `sub` rule and its group-mode residual, and D7's outage
  behaviour. Placeholders only (`example.com`).
- [x] Build the docs site, which generates the configuration reference.

### Task 5: live proof

A real Google OAuth web client on a Workspace domain, with the loopback redirect
`http://127.0.0.1:8766/auth/callback`, and Dispatch run locally from this branch with the four
domain-mode settings (`running-dispatch.md`, "by hand"), with no `AWS_*` variable and no AWS
configuration reachable. One browser profile per account. Record:

- [ ] **Boot without AWS:** Dispatch boots and logs the WARN line "meaning search unavailable"
  (`cmd/dispatch/main.go:278-286`).
- [ ] **Member:** a domain member signs in, and `/auth/whoami` names them.
- [ ] **Outsider:** an account outside the domain reaches Dispatch's refusal page with its reason,
  not Google's chooser. If the `hd` hint keeps the account from being picked, delete `hd` from the
  authorize URL by hand, which also proves the check runs on the server.
- [ ] **`prompt=consent`:** after `POST /auth/logout`, signing in again succeeds. Negative control:
  on one more sign-in, delete `prompt=consent` from the authorize URL and observe the 502 "the
  issuer returned no refresh token".
- [ ] **A refresh that passes:** set `people.confirmed_at` two hours back in the local Postgres, as
  the tests do through `ageConfirmation` (`settings_test.go:444-459`), make one request, and record
  that `confirmed_at` moved past the aged value and `whoami` answered 200: together these show
  `Refresh` got a verified `id_token` and the domain rule passed on it.
- [ ] **Revocation:** revoke the app's access in the Google account, age the confirmation again, and
  record that `whoami` answers 401, `user_sessions.generation` advanced, `people.refresh_token` is
  null, and the WARN line "sign-in ended" carries the refresh error (`membership.go:96`).
- [ ] Outcomes and durations go in the PR body under D8. The client is the deploying
  organization's; creating it is a step for its Workspace admin.

## Hardening ledger

(empty)

## End-to-end verification

| Scenario | Surface | Proof |
|---|---|---|
| A Workspace member signs in with Google; an outsider is refused by Dispatch | local Dispatch from the branch, real Google client | Task 5 |
| Dispatch runs without AWS | the same run, no AWS configuration | Task 5 |
| The hourly confirmation and revocation under Google | the same run, aged `confirmed_at` | Task 5 |
| A reassigned address ends the old holder's sessions | `oidctest`, two subjects | Task 3 |
| Cognito sign-in unchanged | the unit tests' unchanged assertions; after the release reaches the existing Cognito deployment, its production check signs in, reads `whoami` and passes one aged confirmation | Tasks 1-3, production check |

## Contract change census

- Search: `rg -n 'DISPATCH_SIGNIN_'` over this repository and over the deployment repository's
  Dispatch configuration. Dispositions here: `deploy/compose/dispatch.compose.yml:40-43`,
  `dispatch.env.example:33-37`, `deploy/README.md:148` gain the domain (Task 4);
  `main_test.go` and `settings_test.go` change as D1 lists; `routes/router.go:380`'s 503 text
  changes as D1 lists; `skills/dispatch/references/api.md:30-31` is updated in Task 4 to say the
  redirect is registered on the sign-in client (the pool's app client, or the Google web client);
  `packages/dispatch/e2e/run-server.sh:89` is checked in Task 4; `packages/envoy/infra` names none
  at `1a2da912`. The deployment repository
  sets the four Cognito settings and is accepted unchanged.
- A new setting, `DISPATCH_SIGNIN_DOMAIN`; the all-or-none rule becomes "issuer, client id, secret
  and exactly one of group or domain". The new refusals close misconfigurations no working
  deployment has: a group under Google's issuer admits nobody (`person.go:30-34`), and a domain
  under another issuer admits nobody. Rollout: immediate.
- A new nullable column, `people.sub`, written only in domain mode; its migration ships with its
  census file.
