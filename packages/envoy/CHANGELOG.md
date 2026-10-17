# Changelog

## [Unreleased]

### Added

- The secrets broker reads its agent secrets from Google Cloud Secret Manager as well as AWS
  Secrets Manager. `BROKER_SECRETS_STORE` picks the store (`aws-secrets-manager`, the default when
  unset, or `gcp-secret-manager`); with the GCP store `BROKER_SECRETS_PREFIX` is
  `projects/<project number>/secrets/<id prefix>-`, `BROKER_SECRETS_KMS_KEY_NAME` names the
  agent-secrets Cloud KMS key in place of `BROKER_SECRETS_KMS_KEY_ARN`, and a secret's owner and
  tier are its annotations. The broker signs in under Application Default Credentials (on GKE,
  Workload Identity for its ServiceAccount; no key file) and needs only `roles/secretmanager.viewer`
  on the project and `roles/secretmanager.secretAccessor` on the namespace's secrets. `GET
  /v1/settings` names the store (`store`) and answers each store's own fields (`kms_key_name` and
  `gcp_project` for GCP), the other store's omitted. `secrets.Store` is the one interface the
  policy and the value release read through, with `secrets.AWS` and `secrets.GCP` behind it;
  `policy.Secret.ARN` is `Source`, the same bytes in the policy version's digest.
  `agent-secrets secret` manages both stores: against a GCP broker it reads and writes Secret
  Manager itself under the person's own Application Default Credentials, refusing a service
  account's.
- `ENVOY_NATS_SOCKS5_PROXY` sends every NATS connection of the bus through a SOCKS5 proxy, as
  `host:port`: the first, nats.go's reconnects in place, and the replacement a client's recovery
  dials, for `envoy-listener`, `envoy-dispatch` (a row of its settings table), `natstail` and the
  MCP bridge. The proxy is handed each server's host name as the URL spells it, unresolved, so a
  proxy that resolves names this process cannot (a userspace Tailscale container's SOCKS5 server
  and MagicDNS) reaches them. A value that is not `host:port` (whitespace around it, a scheme, an
  IPv6 address with a zone, a host that is neither an IP address nor a DNS name, a port outside
  1-65535, or empty) is refused before anything dials, and so is one that holds an nkey seed as
  `bus.HoldsSeed` finds one (the value, its host, a label or part of one, or split across labels by
  `.`, `-` or `_`, in either case), which is a valid DNS label but never a proxy address. A set
  proxy is remote reach, refused without `ENVOY_ALLOW_REMOTE_NATS=1`, since
  the proxy chooses the server. No refusal prints the value. A connect through the proxy that
  fails, its attempts spent or its deadline reached, names the setting and the proxy's address,
  with the proxy's own dial error where nats.go alone would report only `nats: no servers
  available for connection`, and a proxied dial ends at a connect's deadline as a direct one does.
  Unset, the bus dials NATS directly, as before.
- Dispatch signs people in with Google directly for one Google Workspace domain, with no Cognito
  sign-in pool and no AWS account: `DISPATCH_SIGNIN_DOMAIN` takes the place of
  `DISPATCH_SIGNIN_GROUP` beside the issuer, client ID and client secret, under the issuer
  `https://accounts.google.com` and a Google web client. A person signs in when their Google ID
  token's `hd` is the domain and Google has verified their `email`, and is named by that email;
  anyone else reaches the refusal page with the reason (not in the domain, email not verified, no
  email). The authorization request asks Google for offline access with `prompt=consent` and the
  domain as `hd`, and the hourly confirmation applies the same rule to the refreshed token. The code
  flow reads `email`, `email_verified` and `hd` from the verified token in domain mode alone; the
  shared verifier, which Cognito tokens, pods' tokens and the listener go through, is unchanged.
  Migration `0112` adds the nullable `people.sub`: in domain mode a sign-in records the Google
  account's `sub`, and a sign-in under another account for the same address (a Workspace admin
  gave a deleted account's address to a new one) ends every session of the address in the
  transaction that updates the locked person row, as does a confirmation whose refreshed token
  names another account.
  Exactly one of the group and the domain is set: both, a domain under another issuer, a group
  under Google's, and a domain that is not a lowercase DNS name refuse the boot, and header identity
  and dev sign-in refuse the domain. A deployment with the four Cognito settings boots, signs in and
  confirms as before; its refusal texts for partial settings now name the domain beside the group.
  The compose deployment passes `DISPATCH_SIGNIN_DOMAIN` through.
- Every live session now carries `host` beside `machine_id`: where the session process actually
  runs, as its own plugin reported it (envoy-client's `machineID()`), rather than whichever
  listener happened to register it. Sessions on separate machines registering through one shared
  listener previously all showed that listener's own `machine_id` as their machine. `machine_id`
  keeps its routing meaning unchanged (the listener that owns the session's delivery,
  `store.Registry.Match`, `session.go`'s `hostFor`); `host` is carried on `POST
  /v1/interests/subscribe` (`subscribeBody.Host`, every subscribe and heartbeat), stored on the
  session entry (`session.SessionEntry.Host`), and returned from `GET /v1/sessions` and `GET
  /v1/roles/<role>`, falling back to `machine_id` (`SessionEntry.HostOrMachine`) for a session
  whose plugin predates the field. `GET /api/v1/agents` carries it through unchanged, with its
  own defensive fallback to `machine_id` for a listener that predates the field too.
- `deploy/compose/single-node.compose.yml` runs Envoy on one machine: a JetStream NATS of its own
  (its data on the `nats-data` volume) and the listener, both on `127.0.0.1` only, with
  `deploy/compose/single-node.env.example` as its settings template. The site's Envoy page ("Running
  Envoy on one machine") walks through starting it and pointing a machine's sessions at it.
- A human's answer to a decision block carries his approval forward (LEGION-573). When the
  version his answer writes differs from the version he approved only in its ask blocks, every
  block it holds that is not as the approved version held it is one he answered, and at least one
  is, Dispatch records an `approved` review in his name pinned to that version and appends
  `artifact.approved`, in the transaction that writes the version (`docs.carryApproval`, from
  `writeVersionTx` after the open request's move), rather than leaving his approval stale and
  asking him to approve again what he just decided. The carried review's `reason` is `Carried
  forward from the approval of version <N>: the answer to a decision block was the whole change
  since it.`; an approval request open at the carry is answered `Approve` in his name
  (`ask.answered`) and the review's `ask_id` names it, null otherwise. The new version is compared
  with the one he approved alone: a block of it not as that version held it must be his answer,
  given after that approval, one must differ, and no version between them is read. Who answered
  what is the document's asks rows, never the blocks' own attributes, which a peer on the document
  socket writes as it likes: an ask block no row of the document backs (forged, or not yet
  indexed), a block id the version repeats, a block whose server attributes are not what
  settlement's own writer leaves from its row (`state`, `answered_by`, `answered_at`, `selected`,
  `answer`, a copy's `copied_from`, an unreadable block's `invalid`; an answer settlement withheld
  for room is as the row has it), and the block of an answered or resolved row that asks other
  wording than the row was decided on (the wording settlement writes back) or that settlement
  cannot parse refuses the carry; an open row's body is free. The comparison is
  `docs.CutDecisionBlocks`, the markdown rendered again with every ask block left out wherever it
  stands, on both sides. An edit outside the blocks, a block he did not answer (open, resolved, or
  answered by someone else, whether added since his approval or held open by the approved version
  and answered or resolved since; a block answered before his approval and untouched since is
  content he approved), a block of the approved version the agent removed, an agent's reword of an
  answered block or a selection forged on one landing in the version his answer writes, an
  answered block the agent deleted and inserted again (its row stays answered; nothing of his
  changed, whether the approved version held the block or not), two people's answers in one
  version, an approval by someone else only, his own later request for changes or anyone's newer
  one, a copy's document when its source is answered, and a copy planted in the document since his
  approval carry nothing, as before; should two people's approvals both pass, nothing is carried.
  An approval request on a document a human approved earlier names that approval in
  `ask.approval.approved` (`{version, at, by}`), so the human reads what he is approving since the
  version he approved; a first request carries no `approved` key.
- A finished Actions job on the default branch, of a run with no pull request, is published on
  `notifications.github.<owner>.<repo>.job.<run_id>.completed` (LEGION-690): a completed
  `check_run` that lists no pull request, from the GitHub Actions App, whose
  `check_suite.head_branch` is the repository's `default_branch` and whose `details_url` names
  `/actions/runs/<run_id>/job/<job_id>`. The payload is `kind: "job"`, `repo`, `run_id`, `job_id`,
  `name`, `status`, `conclusion`, `started_at`, `completed_at`, `head_sha` and `url`, each text
  capped like every envelope's. A check run that lists a pull request still goes only to the CI
  store, a merge queue's or another branch's job is published nowhere, and `job` joins
  `contracts.GithubTopicKinds`, so the listener's subscribe warnings take it as a kind.

- The Delivery and Pipeline pages update themselves live (LEGION-690). `GET /ws/delivery` (any
  signed-in person or agent token) takes a `subscribe` frame naming a view, the GET route's query
  string and a preset or custom window, and answers a `snapshot`, the matching GET route's body
  with a revision per key; after that it sends only `delta` frames carrying what changed (pull
  requests, runs and session groups by id, map entries by key, counts by facet and value, the
  other fields whole), a `freshness` frame when the freshness verdict changes, and a `heartbeat`
  every 15 s, the only frame on a quiet window. Nothing re-reads on a timer, a preset's end stays
  at the present, and a reconnecting page resumes with its last change and revisions and is sent
  what changed since. A jobs read marks a run complete only when that same GitHub listing has every
  job finished, and Dispatch shutdown waits for delivery sockets and the live hub before it closes
  its database pool. Under `DISPATCH_TEST_HOOKS=1`, `POST /api/v1/delivery/_test/disconnect`
  closes every such socket. `packages/dispatch/e2e/delivery-live-probe.ts` watches the route under
  a bearer, one JSON line per frame, and compares what it applied with the GET routes.

- Dispatch has a Replies page (LEGION-687): every thread a person wrote in - an ask's thread, a
  comment thread, an issue message thread - where someone else has written since they last read
  it, newest reply first, with a `Replies N` count in the sidebar and, below 1280 px, the compact
  header. A reply, a message, or an answer someone else gives to an ask counts; the row names who
  wrote it, and an answer reads as `answered` with its chosen options and text. Reading the thread
  on screen, replying in it, or **Mark read** clears the row, and a later write brings it back; an
  open ask the Inbox lists stays off the page, and the Inbox is unchanged. Migrations 0095-0099
  add `user_threads`, kept by triggers on `comments`, `asks` and `messages`, seed it from history
  with every thread read, so the deploy marks nothing unread, and catch up any write that landed
  while the seed ran. `GET /api/v1/me/replies` lists the threads and
  `PUT /api/v1/me/threads/{id}/read` moves one forward, appending `user_thread_state.updated`,
  which never notifies. Opening a link to a comment (`/issues/<KEY>/comments/<id>`) now opens its
  thread.
- A table's entry in `GET /api/v1/artifacts/{id}/blocks` (and the issue- and project-slug forms)
  lists its rows and cells as `table.rows`: each row's block id and index (the index `delete_row`
  takes, 0 the header) and each cell's block id, row, column (its index in its row, the index
  `delete_column` takes, so a cell spanning columns or rows counts once), the opening words of the
  header drawn above it, and its own. They are placed by the same walk of the same live tree as
  `GET /api/v1/artifacts/{id}/blocks/{block_id}`, so the two routes agree for every row and cell,
  and the list still holds one entry per table. The document's tables are laid out in document
  order on the one budget of span cells a render of it spends, so a cell's header is the one `GET
  .../text` draws above it, and the block route places a cell under that same header; a table past
  the budget is listed as its markdown draws it, its spans adding nothing (LEGION-693).

- An agent session creates a Dispatch project (LEGION-684). `POST /api/v1/projects` accepts a
  bearer as well as a signed-in person, so a session with its token makes the project its work
  needs, with its `project.created` event naming that session, rather than asking a person to
  make it in Settings. A key that already exists is still `409 PROJECT_EXISTS` whoever asks, and a
  key that is not a project key or an empty name `400 INVALID_PROJECT`. `GET /api/v1` lists the
  route as `any`. A name is at most 80 characters (`contracts.ProjectNameMax`, UTF-16 units after
  the trim; past it `400 CAP_EXCEEDED`), and is `400 INVALID_PROJECT` when it holds a control
  character (Unicode Cc) or a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), or
  has no visible character (a letter, number, punctuation or symbol other than the blank fillers
  U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned,
  such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point
  ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF);
  combining marks, format characters, private-use characters and whitespace don't count); every
  other format character (an emoji's joiner, a flag's tags, a direction mark, a soft hyphen) and
  every combining mark is allowed beside a visible one. Any bearer
  writes a name into the project list every dashboard reads and no route renames a project. Every route
  that refuses a project key that is not one (creating a project or an issue, mapping a repository,
  search) now says the rule one way, `<field> "x" is not a project key: an uppercase letter, then 1
  to 9 uppercase letters or digits, such as CORE`, from `contracts.ProjectKeyRule`, and the key
  pattern itself is generated as `contracts.ProjectKeyPattern`. Creating an issue without a title
  answers `title is required`.
- The Delivery page groups agent sessions (LEGION-682). A session group, kept in two new tables
  (migration 0093), is a stable id, a name and the sessions it holds; a pull request whose first
  session is a member has the group as its Parent agent, `group:<id>`, so the timeline's facet,
  colours, lanes, list and measures all follow the group while the Session facet keeps each
  session apart. `POST /api/v1/delivery/session-groups` and `PATCH` and `DELETE
  /api/v1/delivery/session-groups/{id}` write them on the delivery settings' rule (any signed-in
  person or agent), refusing a session another group holds (`409 SESSION_IN_GROUP`), a name another
  group has (`409 SESSION_GROUP_NAME_TAKEN`) and a name that is the title of a session outside the
  group (`409 SESSION_GROUP_NAME_IS_SESSION_TITLE`). A session that takes a group's name as its
  title afterwards shows as `<title> (<session id>)`. The timeline gains `session_groups`,
  `session_titles` and `session_pr_counts`. The dashboard edits groups from the Parent agent
  facet's **Edit groups**, chooses which Parent agents get their own colour and lane (the rest
  share **Other**), and sets the window from its header: the last 7, 14, 28 or 84 days, rolling,
  or a custom range of whole UTC days, refused past 84 days. A facet change no longer writes the
  window into the address as fixed dates.

- Dispatch's **Machine logins** list shows each login's **Credential** id, the broker's
  `credential_id` that every enrollment the login made names. A login's machine name is what its
  machine reported, so two logins of one service on one host read alike there; the id tells them
  apart, and the broker's operate page uses it to find the login behind a token-less renewal.
- `Ctrl+Alt+M` (`⌘+⌥+M` on a Mac), Google Docs' shortcut, comments on the text selected in a
  document, as the selection bar's **Comment** does: it opens the margin's composer on the
  selection and puts the cursor in it. It works in an issue's documents and a project's documents
  while the cursor is in the document and text is selected, and does nothing in a comment being
  written or on a document the reader cannot edit. The `?` help lists it under **Document** and
  shows `⌥` for Alt on a Mac. On a Mac, shortcuts that hold Alt also match the key's position,
  since Option changes the character (Option+M types `µ`); every other system keeps
  character-only matching, so AltGr continues typing its layout's character (LEGION-680).
- Every document version now carries `size`, the bytes of the markdown it stores, as file and
  image versions already carried theirs: on the issue and project artifact lists, the issue read,
  an artifact's read, each version's read, the reference closure and outgoing reference reads, the
  version an upload, a named version or an edit answers with, and the version on every
  `artifact.created` and `artifact.version` event, settlement's and a rebuild's included. Dispatch
  reads it from the version row's own stored length (`octet_length`), so listing a document near
  the 1 MiB cap neither renders it nor loads its stored history, and a person or a monitor can find
  such a document from the list alone (LEGION-481). The dashboard's issue Artifacts tab and project
  Documents tab show each document's latest size beside its kind, and a document's version list
  shows every version's.
- The conversation view (`/agents/<id>/live`) completes `/` from the session's commands in Send
  and Aside: built-ins, extension/custom/skill commands, file commands and prompt templates. It
  ranks prefix matches first, puts terminal-only rows last and never offers a list in BTW or when
  the session sent none. Picking runs the command as if typed at the session's terminal; output,
  or why a terminal-only command did not run, is the session's Reply via Dispatch (LEGION-394).
- A person lists and ends their own machines' logins and their live grants from their own shell.
  `agent-secrets machine list` and `machine revoke ID`, and `agent-secrets grant list` and
  `grant revoke ID`, call four new broker routes, `GET /v1/operator/machines`,
  `POST /v1/operator/machines/{id}/revoke`, `GET /v1/operator/grants` and
  `POST /v1/operator/grants/{id}/revoke`, which take the machine's own login (a launcher proof)
  rather than Dispatch's UI token and act for its operator, the person who approved the login. The
  grant routes answer and decide as Dispatch's Live grants page does, a revoke of a grant the
  operator's own session got without asking withholding it. The machine routes cover the person's
  own machines' logins alone: their rows of Dispatch's machine-login page, byte for byte, while a
  service's login, such as the Legion daemon's, is neither listed nor revocable there
  (`404 NOT_FOUND`) and is revoked on that page. A service's login, which has no operator, is
  refused every route, `403 SERVICE_CREDENTIAL`. The helper signs these calls with the machine's
  credential through a new `sign-launcher` op, for those four routes under its own broker alone
  (`BAD_REQUEST` otherwise). It refuses `IN_SESSION` to a process inside a registered session's
  process tree, and to one whose ancestry it cannot follow to init (a parent it cannot read, a
  loop, or past 64 processes), so an agent's own commands cannot act as its operator. The check
  covers the session's process tree only: a process the session sends out of it (`( cmd & )`,
  `setsid -f`, a tmux server it started) passes, and the same user can stop the helper anyway. Such
  a process can list and revoke the operator's own machine logins, never a service's machine
  login, and the operator's grants, which include grants the operator approved on any session. It
  can also enroll a box through the helper (`agent-secrets enroll --helper`, open to any process of
  the user) and read the operator's agent secrets. A revoke made this way records
  `launcher:<credential id>` as its actor on every row it writes; a revoke from Dispatch still
  records `human:<email>`.
  `machine list` prints the credential, host, approver, issue and expiry times and state,
  `grant list` the grant, secrets, how it was granted, approver, session, operator and expiry, and
  `--json` prints the broker's body verbatim. `machine revoke` warns on stderr when it ends this
  machine's own login, its id typed in any case.
- A request for agent secrets can ask for a shorter grant: `agent-secrets request NAME... --ttl 2m`
  and the exec form's `agent-secrets NAME... --ttl 2m -- cmd` sign the lifetime into the request
  object (`lifetime_seconds`), the record body carries it (`lifetime_seconds: 120`), and the grant
  ends that long after it is granted while its session lives on. `--ttl` takes a whole number of
  seconds from 1s to 2147483647s; `0`, a fraction of a second or anything else exits 2 before any
  broker call. `POST /v1/requests` and `GET /v1/requests/{id}` answer the `lifetime_seconds` the
  grant got (null for none), and the CLI checks it: a broker or helper from before `--ttl`, which
  drops the claim and grants for longer, has the grant or pending request this call made revoked or
  cancelled at once, and the command exits 1 naming why, running nothing. What an earlier call
  made is left alone and named: an approved grant such a broker reuses, and a pending request it
  answers this call joined (`coalesced`). A request with `--ttl` never reuses a live grant of
  another lifetime.
- A pending secret request names whom it waits on. `POST /v1/requests` and
  `GET /v1/requests/{id}` answer `approver`: the approver the request's credential-request record
  names, a person's Dispatch login or `anyone` for a shared secret, read from the record rather than
  the current policy, so it is the person whose Inbox lists the request even after the secret's
  owner tag changes; null when no person decides. `agent-secrets request` and
  `agent-secrets status` print, after a waiting request's id and state, a line naming that approver
  and where they decide it (`waiting for ada@example.com to approve it in Dispatch: <record page>`),
  and the exec form's wait prints the same line in place of `approve or deny it …`. `--json` prints
  the field verbatim; exit codes are unchanged (LEGION-666).
- The secrets broker can sign in to an Amazon RDS or Aurora database by IAM token. When
  `BROKER_DATABASE_URL` names a user and no password and its host ends in `.rds.amazonaws.com`, every
  new pooled connection, and the migration lock watch's own connection, signs in with an RDS IAM
  auth token minted for that user and host from the AWS SDK's default credentials, in the region
  `AWS_REGION`, `AWS_DEFAULT_REGION` or the shared AWS config names (it needs `rds-db:connect` on
  the database user), so no database password exists for RDS to rotate under the broker. Such a URL
  must name that one host with `sslmode=verify-full` and an `sslrootcert` file, or the broker
  refuses to start naming the host, since a token is a password for 15 minutes,
  `sslmode=require` verifies nothing and `sslrootcert=system` holds no RDS CA. A password pgx reads
  for the URL (`PGPASSWORD`, a passfile) keeps it on that password. The Envoy image ships the RDS global CA bundle at
  `/etc/ssl/rds/global-bundle.pem`, outside the system trust store, so no binary in the image
  trusts an RDS CA for any other connection. A URL with a password, or any other host, connects as
  before (LEGION-662).
- `GET /api/v1/me/answers` lists a person's own answers and replies on asks, newest first,
  with whether each answer is still current. `POST /api/v1/asks/{id}/answer` takes
  `expected_answer_at` to change the current answer; the change is another `ask.answered`
  carrying `previous_answer`, and `GET /api/v1/asks/{id}` lists the ask's `answers`. Migration
  `0082_answers_by_person` adds the partial indexes the list reads (LEGION-622).
- An agent's conversation owns the files and images sent in it, a third artifact owner beside an issue and a project: `POST /api/v1/agents/{session_id}/artifacts` takes a multipart upload with an issue upload's caps, errors and file-store write, and `.../artifacts/{slug}` and `.../artifacts/{slug}/versions/{n}` read one and its bytes; there is no list route. Such an artifact carries `session_id` (null on every other artifact), `ref_key` `agent/<session_id>/<slug>`, and is addressed `dispatch://agent/<session_id>/artifact/<slug>[@vN]`, which the reference graph indexes inside a picture's `![name](…)` as anywhere else, as it does the dashboard page `/agents/<session_id>/artifacts/<slug>[?v=N]`. It holds files and images only: a markdown document is `400 ARTIFACT_INPUT`, its file takes no comment, ask or subscriber (`400 ARTIFACT_AGENT_OWNED`), its upload appends no event, and a session id holding `/`, `?`, `#`, whitespace or a control character is `400 INVALID_SESSION_ID`. Every file or image version is now served with `Cache-Control: private, max-age=31536000, immutable`, since its bytes never change; a document version is not. Migration `0071` adds the column, the one-owner check and the partial index `artifacts_session_id`, which the upload's lookups of a conversation's artifact by name and by slug read, and turns `ref_key` into a trigger-filled column without rewriting the table; its census answers `0` (LEGION-541).
- `POST /api/v1/issues`, `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks`
  now return `advice.suggestions`: the three fused search hits (sjawhar/legion#1764) most like
  what was just filed, and, for an ask, any already-answered ask that settles the same question,
  with who answered and when. Search runs after the write has already committed, scoped to the
  write's own project and bounded by `writeSuggestionTimeout` (300ms), so a slow or down search
  never holds up or refuses a write; `suggestions.missing` says why instead. Everything the
  write's owner holds is left out (the new issue and its spec; the issue or project document an
  ask sits on and everything inside it), and a hit on an issue that is `done` ranks below every
  hit on an open owner, so an issue already closed as a duplicate never displaces the open issue
  it was closed into. Every offered suggestion is recorded in a new `write_suggestions` table,
  whose `outcome` the new `api.RunSuggestionOutcomeSweep` background loop (every
  `SuggestionSweepInterval`, a minute) advances from `ignored` to `acted_on` (the suggested item
  was cited from the source, or the suggested issue was updated directly) or `overridden` (the
  source instead got further activity), so how often the suggestion was right can be counted
  later (LEGION-550).
- `DISPATCH_FILE_STORE_BUCKET` moves uploaded files (images, attachments; never a document's markdown) out of Postgres into an S3 bucket, under `files/sha256/<hash>`, one object however often a file is uploaded, through the AWS SDK's default credential chain (a configuration naming no region refuses the boot). An upload writes its object before its row and before it takes any lock or connection, within 30 seconds; the row keeps the file's name, type, size and hash and no bytes, and a store that refuses answers 502 `FILE_STORE_UNAVAILABLE` and writes no version. The version route serves a row still holding bytes from the row and any other from the bucket: it opens the object within 30 seconds, sends its headers and `Content-Length` only then, streams the body at the client's pace while checking it against the row's hash, and cuts a body that fails or hashes wrong mid-stream rather than ending it as if whole; a bucket that does not answer is 502, an object the bucket does not hold is 500 `FILE_MISSING`, and a cleared row on a server with the setting unset is 503. `/healthz` reports the bucket as `files` (null when unset, else whether a two-second `HeadBucket` answered, probed beside the database probe) and never fails on it, since one task's probe decides whether Dispatch is up at all. `envoy-dispatch backfill-files` moves the files uploaded before the bucket, oldest first and one row at a time, writing each object, reading it back against its hash and only then clearing the row; a row it cannot move is printed `FAILED version <id>` and passed over, the run exits 1 when any failed and can be run again at any time, the server serves each row until it is cleared, and a second run at once is refused by an advisory lock. `--verify-only` reads back every cleared row and exits 1 naming any whose object is missing or wrong; `--restore` is the rollback, writing every cleared row's bytes back from the bucket so a server without the setting, or an image from before it, serves every file from its row. Unset, every upload stays in Postgres as before. The store's tests run the real client against a loopback fake and against an S3-compatible server in a container (`internal/tests3`, SeaweedFS, since MinIO's image left Docker Hub) (LEGION-520).
- `DISPATCH_ASSET_STORE_BUCKET` lets Dispatch serve a missing, content-hashed `/assets/*` file from the same key (`assets/<file>`) in a retained-assets bucket, so a tab left open across a deploy keeps loading its build's chunks. Dispatch reads the whole object, at most 8 MiB, within three seconds before it answers, keeps the immutable cache contract on a hit, and never serves a page or non-asset path from the bucket. An absent object is still a 404; any other store failure, a short body or an object over the limit included, is a 502 with no cache header, logged at ERROR with its key. At most 64 MiB of retained objects are held at once, since a client that stops reading keeps its object in memory: a request waits within its three seconds for room, so a stale tab's burst of chunks is served in turn, and one that cannot get room in time, or whose admitted fetch then runs out of that same bound before the bucket answers, is a 503 with `Cache-Control: no-store` and `Retry-After: 1`, logged at WARN with its key — the shared bound expiring is never presented as a store failure, since the store was only asked too late to answer in time — and a room refusal asks the bucket nothing. A client that disconnects while still queued for room logs no more than a debug line and gets no response. The task role needs `s3:GetObject` on `assets/*` and `s3:ListBucket` on the bucket, without which S3 answers a missing key with 403 and Dispatch with 502. Leaving the setting unset preserves local-only static serving.
- `DISPATCH_AGENT_TOKEN` takes several values separated by whitespace, the first the current one, so the shared agent token can rotate with an overlap: the HTTP API and the document websocket accept every value, comparing a bearer with each in constant time. Startup refuses an empty entry (two whitespace characters in a row) or a repeated one, naming its position and never its value; one value behaves as before. A request that authenticates with a value after the first logs `dispatch: request authenticated with a previous shared agent token` at WARN, with the value's position, the rightmost `X-Forwarded-For` address (the connection's own without one), the User-Agent and the path, at most once per address and User-Agent every 10 minutes (LEGION-538).
- `envoy-dispatch settings` prints every Dispatch setting the server and its subcommands read, one row each with its `_FILE` form, default, whether it is required and a one-line description, from one table (`cmd/dispatch/settings.go`) that is now the only place Dispatch's own code reads its environment; the docs site's Dispatch configuration reference is generated from it. The libraries Dispatch links still read their own variables (`HOME`, libpq's `PG*`, Go's proxy, certificate and runtime variables), which the table does not list. Every setting resolves as before, the `envoy.json` overrides and `_FILE` forms included; the readers that used to call `os.Getenv` themselves (the dashboard directory, the GitHub App credentials, the signing key and insecure-cookie flag, the `envoy.json` overrides, the Envoy listener token, and NATS's reach and nkey) are handed their value from the table.
- `envoy-dispatch routes` prints the route table `GET /api/v1` serves, read without a database or a listener; the docs site's Dispatch HTTP API reference is generated from it.
- `envoy-dispatch census`: the pre-deploy census of the migrations a database has not recorded (the runner's own rule), for a deployment to run before it rolls the service. It prints, for each pending migration, the tables it locks above ACCESS SHARE with size, row count and the sessions holding locks on them (pid, role, application, state and transaction age, "not visible" where Postgres hides one, or "autovacuum worker"; never query text), the transactions open longer than a minute or of an age it cannot see, and the count the migration's own `<version>_<name>.census.sql` answers. Those tables are the ones its statements name (an aliased `update` and a foreign key's referenced table among them; none in a comment, a string literal or the body of a function the migration defines), the table behind an index it drops or alters, and every table a foreign key reaches from rows it writes (a cascade, or a check that a row is still referenced), read from `pg_constraint` and from the keys earlier pending migrations add, inside a `DO` block too, and checked for holders and readability but not counted. It refuses (exit 1) a census that counts rows; a table a migration names above 1 GiB, which it does not count; a lock on a touched table held by a transaction older than a minute, or by one whose age Postgres hides from the census's role (granting that role `pg_read_all_stats` lets it read the age) or does not record (`track_activities` off), but not by an autovacuum, which Postgres cancels for the migration, unless it is anti-wraparound, or may be (its activity hidden or untracked, its table past its freeze age), or `deadlock_timeout` is not shorter than the lock timeout; a touched table it cannot read within the five-second lock timeout or the statement timeout; a census that fails, a name the database lacks included, even behind an earlier pending migration that may create it; and a database that records no version yet holds tables. A fresh database, one recording no version and holding no table, passes with `census: fresh database, nothing to check`. A failure names the file and the SQLSTATE, and Postgres's message only when it points into the census's own text, so no row value reaches a log. The census exits 2 when it cannot be taken (a connection whose `search_path` names no schema that exists among the causes), and writes nothing: one read-only transaction, the census statement sent through the extended protocol whatever the connection string asks. `pgmigrate.Load` refuses a census that is not one select, that holds a Unicode escape (`U&'…'`, `U&"…"`), or that names `pg_terminate_backend`, `pg_cancel_backend`, `pg_sleep`, an advisory-lock function (`pg_try_advisory_*` included) or a function that runs a query given as text (`query_to_xml` and its kin, `ts_stat`, `ts_rewrite`), bare or quoted and in any case, anywhere outside its comments and string literals, which it finds as Postgres 16's lexer does; the census runs with `standard_conforming_strings` on, so Postgres reads its literals the same way. Every migration from `censusRequiredFrom` (56, `internal/dispatch/store/store_test.go`) declares a census; 0053, 0054 and 0055 carry worked examples. Each store's tests hold the census's reading of every migration to the locks it really takes, run every shipped census at the schema just before its migration, and require a census that reads what a migration from `censusRequiredFrom` on creates, renames or gives a new type to name that migration, since a release carrying both refuses every deploy. An unknown `envoy-dispatch` subcommand now exits 2 instead of serving, which migrated the database (LEGION-459).
- `DISPATCH_DEV_SIGNIN=1` mounts `GET /auth/_dev/signin?login=<email>&next=<path>` on Dispatch, which signs the person it names in, lowercased, with the same session cookie a sign-in-pool sign-in issues, with no pool step, so a local instance can be driven signed-in. Boot refuses the flag unless identity is cookie, no `DISPATCH_SIGNIN_*` setting is set, the listen address is a loopback IP literal, the dashboard origin names `127.0.0.1`, `[::1]` or `localhost`, every `DATABASE_URL` host is loopback or a unix socket, `DISPATCH_SIGNING_KEY` is unset, `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, a set `DISPATCH_AGENT_SECRETS_URL` names a loopback host, `ENVOY_URL` names a loopback host, and a loaded GitHub App private key comes from `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback host, since a signed-in session can have the App probe and import any repository it is installed on. A key in `app.json`, where a developer keeps the real App's key, is refused whatever the base, naming the file; the environment's key must be a throwaway, as `packages/dispatch/e2e/run-server.sh` generates one, because every App call hands a signed App JWT to whatever listens at that base. These refusals come before NATS or Postgres is dialled. The signing key is then generated per process, so a minted cookie is worthless on any other server and dies with the process. What a signed-in session writes to the database (a `dsp_` token it mints, the session rows its sign-out changes) is honoured by every server on that database, so a dev-sign-in server needs a database of its own. While the flag is on, every request must carry the dashboard origin as its `Host` (`421 HOST_MISMATCH`), and the route serves only a loopback peer with no forwarding header (`403 DEV_SIGNIN_FORBIDDEN`) and logs every mint at WARN. `DISPATCH_LISTEN_HOST` may now be an IPv6 literal with or without brackets: `::1` listened on `::1:8766` and failed with "too many colons", and now listens on `[::1]:8766`. A bad `DISPATCH_PORT` now refuses the boot before NATS and Postgres connect.
- Every cache warm-up logs one line when its initial scan ends, `<cache> cache warm-up`, with the bucket, `elapsed_ms`, `entries`, `delete_markers` and `outcome` (`completed`, or `timed out` at WARN when nats.go's idle timer gave up on the scan). The counts are disjoint, like the role restore line's: `entries` is the live keys the scan delivered, `delete_markers` what it streamed past to find them. The line goes through the logger the listener passes each cache (`store.WithLogger`, `session.WithSessionLogger`, the CI store's own), so it is a JSON record with `machine_id`, and so are the cache watchers' other lines (a failed first start, a recreated bucket, a watcher that stopped). A restart now names its own cost — measured on a listener with a seeded bucket: `{"msg":"interest registry cache warm-up","machine_id":"smoke374r2","bucket":"envoy_interests","elapsed_ms":5,"entries":3,"delete_markers":500,"outcome":"completed"}`. A timed-out scan is logged and never recorded as the watcher's terminal error, so a live watcher whose warm-up merely timed out is not a 503 or a rebuild.
- A listener start logs one INFO line for the role-claim snapshot it restores, `restored role claims`, with two disjoint fields: `restored`, the claims whose revision the scan read and which therefore keep their holder's restart grace, and `delete_markers`, the tombstones the scan streamed past to find them (production on 2026-09-28: `restored=9 delete_markers=757`). They are separate on purpose — one total of both reads as claims the restart failed to restore. `internal/store`'s lines, this one and both reaper cycles, now go through the logger the listener passes it (`store.WithLogger`), so they are JSON records carrying `machine_id` like the rest of the listener's output. `internal/bus`'s lines and the stdlib `log` package's stay in Go's text format, which the deployed CloudWatch metric filters for publish failures, webhook refusals and dropped stream subjects match on.
- A CI record write that runs out of its two-second retry budget logs one JSON line, `ci record exceeded its retry budget`, naming the head (`owner`, `repo`, `number`, `sha`), the record's `checks`, the write's `attempts`, the `observations` it answered 503 and the last attempt's `error`, so an alarm can count the listener's 503s by that cause.
- Added the native Dispatch workspace API, persisted documents and events, retained Dispatch notifications, and daily Postgres backups.
- `POST /v1/roles/set` accepts `"soft": true`: the claim lands only if the role is unheld, held by a session that is no longer live, or held by the declared `previous_session_id`; any other live holder answers 409 with its id.
- Dispatch redelivers the GitHub App webhook's failed deliveries, which GitHub never redelivers on its own. Every two minutes it lists the webhook's attempts whose status is not OK and asks GitHub to redeliver each one the listener answered 5xx or GitHub could not complete. A failed or refused redelivery is retried after a doubling backoff, at most five times, and a 4xx is never redelivered. Requests go a second apart, and a GitHub rate limit stops the sweep until the time GitHub gives. `envoy-dispatch redeliver-webhooks --since <d> [--dry-run]` runs the same sweep over a chosen window.
- Every Dispatch message read returns `broadcast_id`, the broadcast the message is one
  recipient's copy of, or null: the thread read (`GET /api/v1/messages/{id}`), the Agents page's
  conversation list, each reply, and every message event (LEGION-394).
- `POST /api/v1/messages/{id}/deliveries/{attempt}/accept`: the attempt's session records, once
  per message, that it took a person's fresh, latest attempt of a direct message to it, requested
  by the message's own author, as its user's own turn, and is answered that attempt with the
  message's stored `body`. Anything else is refused with a 409 naming the check
  (`ACCEPT_NOT_DIRECT` for a message on an issue, a broadcast's copy or a reply in a broadcast's
  thread, `ACCEPT_NOT_WRITTEN_BY_PERSON`, `ACCEPT_NOT_ASIDE_OR_STEER` for a BTW,
  `ACCEPT_ALREADY_ACCEPTED`, `ACCEPT_SUPERSEDED`, `ACCEPT_NOT_REQUESTED_BY_PERSON`,
  `ACCEPT_NOT_REQUESTED_BY_AUTHOR` for another person's retry, `ACCEPT_FAILED` for an attempt
  Dispatch recorded as failed, `ACCEPT_STALE`) or 403
  `ACCEPT_FORBIDDEN`. It appends `message.accepted`, which reaches the dashboard's event stream
  and never NATS, since the outbox publishes no issue-less event. Every delivery attempt now reads
  `requested_by` (who asked for it, kept on a resume, null before migration 0054), `accepted_at`
  and `accepted_as` (LEGION-394).
- `POST /api/v1/broadcasts` requires `idempotency_key`, naming one send (letters, digits, `.`,
  `_`, `:` and `-`, at most 128 characters), so a repeated create no longer hands every recipient
  the message twice. A repeat by the same human with the same key, body, mode and `session_ids`
  is answered `200` with the broadcast the key made, even while the listener is down; the same
  key with a different request is `409 BROADCAST_KEY_REUSED`, naming that broadcast as
  `broadcast_id`, and sends nothing; two requests carrying one key at once write one broadcast.
  A missing key or one with another character is `400 BROADCAST_INPUT`, and one over 128
  characters `400 CAP_EXCEEDED`; only the missing-key text adds what a page loaded before this
  change should do (restore its draft, copy the message, reload and send again). Keys are stored
  per human in `broadcast_idempotency_keys` (migration `0055`) and kept as long as their
  broadcast (LEGION-446).
- Each request a listener's starting gate refuses logs `request refused while starting` with the
  `gate` (`webhook` or `v1`), `method` and `path`; a start logs `envoy-listener /v1 open` with
  `since_listening_ms` and `durable bound` with `attempts` and `waited_ms`.
  `packages/envoy/scripts/listener-deploy-probe.sh` watches a listener deploy from a client's
  seat, at every address the listener's name resolves to, and exits 1 when a task refused `/v1`
  while it answered `/healthz`, or, with `--dispatch-*`, when a Dispatch message it posted did not
  record state `sent` (LEGION-456).
- `GET /api/v1/artifacts/{id}/blocks/{block_id}` says where one block stands in a Dispatch
  document: its path from the top-level block down (each node's type, block id and child index)
  and, for a table block, row or cell, the table's id, the row index (0 is the header row), the
  cell's column index (the indexes `delete_row` and `delete_column` take), the text of the header
  cell drawn above it (in a table with colspans or rowspans, the column the cell is drawn in) and
  the row's cells. An id the live document does not hold is `404 TARGET_NOT_FOUND`.
  `GET /api/v1/comments/{id}` and `GET /api/v1/asks/{id}` carry the same answer for their
  anchor's block as `anchor_block`, derived from the live document at read time and absent when
  the anchor names no block or the block has left the document; lists and events do not carry
  it. When the anchor's document cannot be read, those two reads still answer `200`, without
  `anchor_block` and with `anchor_block_error` (`DOC_SERVICE_UNAVAILABLE`, `DOC_SCHEMA` or
  `INTERNAL`, the codes the API answers those errors with elsewhere), logged at WARN, and they do
  not wait for a failed document room's recovery; only a request that has itself gone away fails
  them (LEGION-460).
- `PUT /api/v1/me/agents/{session_id}/state` accepts `read_replies`, the ids of messages that
  session wrote, and marks those replies read and no other (`user_agent_reply_read`, migration
  `0067`); `unread_replies` and the conversation window's unread flag leave them out. Opening a
  broadcast page now clears the answers it shows from the New replies badge and each agent's row,
  on every tab, while the session's older reply to another message, or one newer than the page
  shows, still counts. An id may take any form `uuid.Parse` reads; one it cannot read, or one that
  is not a message that session wrote, is `400 INVALID_STATE`. A request naming only replies
  already read by id, or replies the session's read mark has passed, changes nothing and appends
  no `user_agent_state.updated` event. A reply the viewer's Clear hides but the read mark has not
  passed is neither, though the count leaves it out: a Clear can move back, so the first request
  naming that reply writes its row and appends the event. A `read_through` deletes the viewer's
  rows for that session whose replies it reaches (LEGION-485).
- A Dispatch document edit's response reports `advice.decision_blocks_added`: how many ask blocks
  the edited document holds whose block id no ask block in the document before it carried, read
  from the parsed trees the edit was applied to. A block inserted in a blockquote, on a list item's
  line or behind four or more colons counts, as does a block retyped into an ask; an opener inside
  a fence or indented code counts nothing, nor does a block the edit moved or reworded. An issue
  document's edit carries it beside the issue advice; a project document's edit, which before
  carried no advice, now carries `advice` holding the count alone (LEGION-470).
- The secrets broker logs at boot how it signs in to its database, `database sign-in
  method=rds-iam` or `method=password`, before it reaches for AWS or the database, so an operator
  tells a token sign-in from a password one in the log rather than the task's environment.

### Changed

- The secrets broker's policy keeps a single-name reread against a lagging listing only when the
  reread changed the live answer; a reread that confirms what the policy serves costs the next
  reloads nothing. A grant whose frozen source is no longer the one the policy serves its secret
  from (a secret deleted and recreated under its name on AWS, or a move between stores) answers
  `403 GRANT_NOT_LIVE` and is not reused, where it answered `404 SECRET_NOT_IN_STORE`; the session
  asks again.

- **Breaking:** every listener `/v1` request with a body must set `Content-Type: application/json`;
  parameters such as `charset=utf-8` are accepted. Any other type, a malformed one, or none is
  answered 415 with a JSON error before authentication or the handler runs, whether the body's
  length is known or not (a chunked body is checked too). A cross-site browser request can no longer
  publish a `text/plain` body through an unauthenticated loopback listener. A request with no body,
  such as a `GET` or `DELETE`, is unaffected, and webhook routes keep their own signature and
  content handling. Every Legion client already sends the header; an operator's own script that
  posts to `/v1` without it now gets the 415 and must set the header.

- **Breaking:** `GET` and `PUT /api/v1/settings/delivery` replace
  `production_job_name` with `production_job_names` (a string array) and
  `production_apply_step_name`. `PUT` trims each name and refuses an empty list, a blank name or a
  duplicate with `DELIVERY_SETTINGS_INPUT`. Migrations 0109 and 0110 retain the old database column
  through one rollout while recording the configured apply step and its conclusion for listed jobs.
  A production job that succeeds without that step succeeding does not count as a deploy. Warnings
  name every configured production job and a missing apply step, and the Pipeline card's `job`
  joins the names it follows with `or`.

- **Breaking:** migration 0111 drops `delivery_settings.production_job_name`, with the trigger and
  function 0109 added to keep it at the first of `production_job_names` through the rollout
  (LEGION-761). No value is lost: the trigger kept the column equal to the list's first name, and
  the migration's census counts any row where it did not, so such a row refuses the deploy.
  Rollback floor: once 0111 has applied, a task older than legion-envoy v13.4.1, the release that
  added 0109, still boots, but its reads and writes of the delivery settings fail with SQLSTATE
  42703 (undefined column), since they name `production_job_name`, and so does every delivery page
  and job that reads the settings first.
  v13.4.1 and every later release read `production_job_names` alone.

- **Breaking:** `envoy-dispatch` and `natstail` read `envoy.json` from
  `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json` and the `dispatch` section of the checkout's
  `.envoy/envoy.json`, and take the NATS URLs from `envoy.natsUrls` (LEGION-707). A checkout's file
  that holds an `envoy` section refuses startup, naming `envoy` and the user file it belongs in
  (`envoy: not read from a checkout's envoy.json; put it in <user file>`): a cloned repository
  does not choose the NATS servers the process connects to. The session clients ignore such a
  section and say so in their start notice instead. A `.envoy` that is a plain file, as some
  repositories have, counts as no checkout file. A JSON `null`, at the top level, as a section or
  as any key's value, is refused naming the key (`dispatch: Expected object`,
  `envoy.natsUrls: Expected string array`), as the clients refuse it, and JSON that does not parse
  is refused naming the file, the line and the column, never the text it stopped at, which can be
  the start of a token or seed. A top-level `natsUrls` is refused: a user file's naming
  `envoy.natsUrls`, a checkout's saying a checkout's envoy.json holds only a `dispatch` section.
  `~/.config/opencode/envoy.json` is not read: while it is the only user file and the user file is
  `~/.config/legion/envoy.json`, startup refuses, naming both paths and the move
  (`<old file> is not read: mkdir -p <user file's directory> && mv <old file> <user file>`), whatever
  `DISPATCH_SERVER_URL` and `NATS_URLS` say. A checkout's `.opencode/envoy.json` is not read: while
  the checkout has no `.envoy/envoy.json`, `envoy-dispatch` logs one WARN at start
  (`dispatch: a checkout's .opencode/envoy.json is not read`, with the rename as `fix`) and
  `natstail` prints the rename. Neither gives a `mv` across a mount: where the old file's directory
  and the nearest existing ancestor of the new file's directory are on different filesystems (as in
  an agent box that bind-mounts `~/.config/opencode`), the text is `<old file> is not read, and is
  on another filesystem than <new file> (a mount): create <new file>, and move <old file> on the
  machine that owns it` (`config.LoadOptions.DeviceOf` answers a directory's device, `stat`'s by
  default). The `envoy` section takes `enabled`, `url`, `natsUrls`, `token` and
  `natsSeed` and refuses any other key; the server reads `envoy.natsUrls` whatever
  `envoy.enabled` says, since `enabled` switches a session's Envoy. `XDG_CONFIG_HOME` is a row of
  the settings table, and one set to a path that is not absolute refuses startup, naming it.
  `DISPATCH_SERVER_URL` and `NATS_URLS` still override both files, but not the old-file refusal.
  No Go process runs `envoy.natsSeed`: `natstail` and `envoy-dispatch redeliver-webhooks` connect
  as the nkey user `NATS_NKEY_SEED_FILE` (or `NATS_NKEY_SEED`) names, so a run against a NATS that
  requires a user needs `NATS_NKEY_SEED_FILE`, which the session launchers no longer export.
  The compose deployment (`deploy/compose/dispatch.compose.yml`) mounts no `envoy.json` and needs
  no change.
- **Breaking:** the secrets broker refuses to start when `BROKER_SERVICES` binds a namespace's
  `default` service account (`system:serviceaccount:<namespace>:default`), naming the entry: every
  pod in that namespace whose spec names no account runs as it and can get a token for the broker's
  audience, so the binding proves nothing about which pods are the service's. Bind each service to
  a dedicated account its pods set as `serviceAccountName` before taking this release.
- **Breaking:** a renewal (`POST /v1/enrollments/{id}/renew`) of an enrollment that has ended,
  lapsed or never existed answers `401 ENROLLMENT_NOT_LIVE` where it answered `401 PROOF_INVALID`;
  the renew route alone answers it, and every other session route still answers a not-live proof
  `PROOF_INVALID`, the code that stays for retryable proof failures such as iat skew. A pod's
  renewal carries the pod's current projected token as `pod_token`, and the broker refuses it
  `403 POD_IDENTITY_MISMATCH` unless the token names the pod UID and service account the enrollment
  was made for, and `403 POD_TOKEN_EXPIRED` (naming its expiry, and retried by the renewer with the
  token its file holds next) once it has expired. A machine session's renewal carrying a token is
  `403 POD_IDENTITY_MISMATCH`. The token is not yet required, and the broker ratchets each pod UID
  instead: every pod enrollment, new or found by migration 0014, starts unmarked, and the first
  renewal that carries the pod's token marks it. Once any enrollment naming a pod UID, live or ended, is
  marked, a renewal without the token from any enrollment naming that pod, whichever machine login
  made it, is `403 POD_TOKEN_REQUIRED`; until then it is accepted and logged as `broker: pod
  enrollment renewed without its projected token`. So the copied-token gap closes for every pod UID
  a current-image pod has renewed under, and stays open for old-image UIDs until the release that
  requires the token. A current-image pod still leaves one window: a copied token used before the
  pod's own first renewal with its token. That is seconds while the Legion daemon holds an approved
  machine login; after a daemon restart its pods enroll only once a person approves its new login,
  and a pod that ends before it enrolls never renews with its token, so its UID stays open as an
  old-image pod's does. A pod UID names one pod for ever, so a mark refuses nothing of another pod,
  and it keeps refusing a copy between the pod's role generations and after the pod is gone. The
  renewer stops on `POD_TOKEN_REQUIRED` and `ENROLLMENT_NOT_LIVE` and
  retries `POD_IDENTITY_MISMATCH`, which the broker also answers for a token it could not verify
  while its issuer was unreachable; a real mismatch ends with the lease. `agent-secrets renew
  --pod-token-file FILE` reads the file before every renewal and sends it, and Legion's worker shim
  passes the projected token file. **Rollout:** any order keeps every pod renewing (an older
  broker reads no renew body, and this broker accepts a token-less renewal from a pod no
  enrollment of which has sent one), but it does not bound exposure: a grant lasts until its
  session ends, so a copy renewing an old-image pod's UID keeps one approval's grant for as long as
  it renews. Before or right after the broker deploys, replace every old-image issue pod, then
  revoke the machine login that made each enrollment a remaining token-less WARN line names (the
  line's own enrollment) on Dispatch's machine-login page, by its **Credential** id once its code
  shows it is not the daemon's, never by its machine name, which a copy's login can share (the
  operate page gives the query). The release that requires the token is safe once all three hold:
  `select count(*) from enrollments where kind='pod' and revoked_at is null and pod_token_renewed_at is null and created_at < now() - interval '1 minute'`
  answers 0; the broker has logged no token-less WARN line for one full `BROKER_LEASE_SECONDS`; and
  no issue pod runs a worker image older than the one whose renewer sends the token (an old-image
  issue pod between role generations holds no enrollment, so the count and the log miss it, yet its
  next phase enrolls an unmarked row).
- **Breaking:** only the machine login that enrolled a pod may end its enrollment:
  `DELETE /v1/enrollments/{id}` of a pod enrollment by any other login, another service's or a
  second login of the same service, is `403 NOT_ENROLLING_CREDENTIAL` and ends nothing. A Legion
  daemon that logs in again (after a restart, or when its login reaches its expiry,
  `BROKER_LAUNCHER_CREDENTIAL_SECONDS`, 7 days by default) cannot end its earlier login's pods;
  each ends one lease after its pod stops renewing, or at once when someone revokes that earlier
  login, and the daemon no longer retries that refusal.
- `agent-secrets renew --pod-token-file FILE` re-reads the projected pod token before each renewal
  and sends the token as `pod_token`. A blank or unreadable file posts no renewal: the renewer
  prints `ERROR: enrollment <id> cannot read its pod token (<reason>); nothing was sent, so its
  lease lapses unless the file becomes readable` and retries rather than exiting 76 (at start it
  exits 2 and the shim starts it again). The worker shim passes its projected token file. This
  client works against a broker that predates the renewal body because that handler ignores it.
  The terminal renewal refusals `POD_TOKEN_REQUIRED` and `ENROLLMENT_NOT_LIVE`, and an older
  broker's `PROOF_INVALID` saying `not live`, exit 76 (`EX_PROTOCOL`) naming the enrollment, HTTP
  status and broker code; the shim quotes that terminal line beside its own enrollment and stops.
  Every other refusal retries, `POD_IDENTITY_MISMATCH` among them, which the broker also answers
  for a token it could not verify while its issuer was unreachable.
- `GET /api/v1/references` no longer renders every document that mentions its target to cut each
  backlink's one-block excerpt (LEGION-531; 24.6% of a week's production CPU). Each document
  mention's excerpt - the block holding the target's first mention, its `#b-<id>` jump and its
  markdown cut to 480 characters - is stored on its `refs` row when the mention is indexed, cut
  from the render the version write already makes, so the route reads one indexed query and loads
  no document. Migration `0092_refs_excerpts` adds the three nullable columns (`excerpt_block_id`,
  `excerpt_text`, `excerpt_version`) and the partial index `refs_excerpt_drift`; its census answers
  `0`. `envoy-dispatch` runs a reconciliation that fills every row indexed before it, one batch of
  20 documents per tick. Until it reaches a row, the route answers that mention with the
  document's name, the fallback a mention in no block already got. The excerpts the dashboard
  shows are unchanged: on the 2026-10-10 production dump, all 7,537 document mentions match the
  rendered path. In the rolling deploy that ships 0092, a version the outgoing task writes keeps
  the previous version's excerpt until the reconciliation's next tick, within five minutes.
- **Breaking:** an agent session on a machine its person logged in is one enrollment kind,
  `session`, where the broker recorded `host` for a session the helper enrolled for itself and `box`
  for a key a launcher such as agentbox brought; a pod stays `pod`. Migration
  `0012_one_session_kind` rewrites every stored `host` and `box` row to `session` and allows only
  `session` and `pod`. Every place the kind is shown says `session`: `GET /v1/enrollments/self`,
  the grant and record reads (`agent-secrets self`, `grant list`, Dispatch's credential page and
  Live grants), and a new record's enrollment line, while a record written before keeps its body,
  which its id hashes, verbatim. A reader matching `host` or `box` must match `session`. The
  clients in the field keep working through the rollout in either order: the broker records an
  older helper's `host` or `box` as `session` and logs `an enrollment named a legacy kind`; this
  helper asks an older broker, which refuses `session` with `400 INVALID_KIND`, once more as
  `host` or `box`. The broker is forward-only from this migration: a broker before it inserts
  `host` and `box`, which the new constraint refuses, so roll forward rather than back.
- **Breaking:** a grant lasts until its session ends unless its request asked for a shorter
  lifetime (`--ttl`, above); there is no fixed lifetime and no cap. `BROKER_MAX_GRANT_SECONDS`
  (default and most 43200, 12 hours) is removed: a deployment that still sets it refuses to start,
  naming the variable and explaining that a request's `--ttl` now chooses a shorter lifetime. A
  grant with no lifetime answers `expires_at: null` on every
  grant read (`GET /v1/enrollments/self`, `GET /v1/grants`, `GET /v1/operator/grants`), and its
  record reads `lifetime_seconds: null` on `GET /v1/credential-requests/{id}` and
  `lifetime_seconds: -` in its body. Migration `0013_grant_until_session_ends` drops not null from
  `requests.lifetime_seconds` and `grants.expires_at`, and every liveness check reads a null expiry
  as one that has not passed. `agent-secrets self` prints `until this session ends` for such a
  grant and `grant list` prints `session-end`; an `agent-secrets` from before this release prints
  `0001-01-01T00:00:00Z` for it, and nothing else changes for that client. Dispatch's credential
  page shows **Until the session ends** and Live grants **When the session ends**. A broker before
  this migration cannot read a grant with no expiry, so roll forward rather than back.
  A running session keeps what it was given across the deploy: a request without a lifetime takes
  the live grant of a request written before this migration (which recorded the old fixed 43200
  seconds), keeping its expiry, and joins such a request while it is still pending. Only rows
  written before migration 13 was applied match, so a later `--ttl 12h` request is never taken for
  one, and nothing matches a day after the deploy.
- **Breaking:** a session has at most one pending request for each secret. `POST /v1/requests`
  answers a request naming a secret one of the session's pending requests names, with another set of
  names or another lifetime, `409 REQUEST_PENDING`, naming the pending request; `agent-secrets`
  exits 1 with that message. This holds for requests without `--ttl` too: a request for `A` and `B`
  while one for `A` waits used to open a second record for the approver, and is now refused. The
  identical request (the same names and the same lifetime, or none) still joins the pending one,
  answered with that request and `coalesced: true`. Wait for the named request to be decided, or
  `agent-secrets cancel` it and ask again.
- The helper's `enroll-box` and `unenroll-box` ops are `enroll-session-key` and
  `unenroll-session-key`, which name no kind, and `agent-secrets enroll --helper` needs no `--kind`.
  Every older client keeps working: the helper still answers `enroll-box` and `unenroll-box`, from
  an `agent-secrets` before this release, as the new pair, logging `a client asked a legacy op`;
  this `agent-secrets` asks a helper before this release, which answers
  `unknown op enroll-session-key`, its `enroll-box` and `unenroll-box` instead, saying so on
  stderr; and `enroll --helper --kind box`, which dotfiles' agentbox passes, enrolls as before
  until dotfiles pins this release and agentbox stops passing it, while any other `--kind` exits 2
  naming the flag, as every `--kind` but `box` did before. An `enroll` without `--helper` is
  refused for that first, whatever `--kind` it carries, as before.
- `POST /v1/enrollments` answers a pod token whose expiry has passed with `403 POD_TOKEN_EXPIRED`,
  naming when it expired (`the projected token has expired (oidc: token expired at <time>)`).
  Before, the broker answered it as `403 POD_IDENTITY_MISMATCH`, "the projected token does not
  identify this pod", which reads as another pod's token. `POD_IDENTITY_MISMATCH` now means only a
  token that does not prove the pod. A launcher that branches on the code presents the pod's
  current token with the same key; the Legion daemon reads the new code as not permanent, keeps
  the key, and enrolls it with the pod's current token, which it asks the shim for at its next
  observation.
- **Breaking:** `agent-secrets launcher login` and `launcher login-status` are removed, with no
  alias: `agent-secrets machine login` and `machine login-status` replace them, beside
  `machine list` and `machine revoke`. A script that still calls `launcher` gets the usage and
  exit 2, so a session launcher gating on `launcher login-status` reads that as no login and starts
  its sessions with no broker identity. Move every caller to `machine login` and
  `machine login-status` before it pins this release. Every message that said to run
  `agent-secrets launcher login`, from the CLI and from the helper, now names `machine login`.
- The secrets broker takes no database password apart from its URL, and refuses to start while
  `BROKER_DATABASE_PASSWORD` is set or `BROKER_DATABASE_URL` names its
  `${BROKER_DATABASE_PASSWORD}` placeholder, naming the variable and why: on Amazon RDS it signs in
  by IAM token, and any other database's password goes in the URL itself, URL-escaped. A deployment
  that still sets either must stop before it runs this broker: put the password in the URL, or move
  to an IAM-form URL on RDS. Going back to the password once a broker runs this release takes the
  reverse order, since a broker on this release exits 1 at boot while either is set: roll the image
  back to legion-envoy v7.6.0 or later, the 7.x releases that sign in by IAM token, which hold every
  broker migration to date and still accept the variable. Once the Legion daemon sends no
  `login_hint` (sjawhar/legion#1870), roll back to v7.10.0 or later instead: earlier brokers need
  that hint on a service's machine login and answer the daemon's next fresh login with a 500. Then
  restore the variable and the `${BROKER_DATABASE_PASSWORD}` placeholder in the URL together.
  Undoing the configuration change that moved a deployment off the variable therefore brings no
  broker up on this release. Before going back, check that the RDS master user does not hold
  `rds_iam`, directly or through a role (if it does, its password sign-in is refused), and that its
  secret's value is current. When IAM sign-in itself is what is broken, make the rollback one
  change: the image and the variable in one task-definition revision, so no broker boots on this
  release with the variable or on 7.x without a working sign-in.
- Anyone signed in to Dispatch decides a service's machine login, such as the Legion daemon's, not
  only the person its request names: the broker reads the service from the signed request, opens
  its record with the approver `anyone` and ignores any `login_hint` it carries, and the decision,
  the credential's chain re-check, the machine-login list and its revoke all follow that service,
  so a login a daemon starts while still naming one person is anyone's to decide, and so is one a
  broker before this change opened naming a person. The pending list keys on the record's stored
  approver, so it shows every new service login to everyone, but a service login an older broker
  opened naming a person is listed for that person alone, though anyone who types its code may
  decide it. Every signed-in
  person lists and revokes every service's login, each row naming who approved it (`approved_by`),
  and Dispatch's machine-login page calls the list **Machine logins**. A person's own machine login
  is unchanged: only the person it names decides it, and a person's login naming no one, or
  `anyone`, is refused `400 REQUEST_INVALID` before any record opens. Every service's login spends
  one shared rate-limit bucket, whatever service it names, and a person's machine login the bucket
  of the person it names (`service`, `person:<login>`), so an invented service name buys no bucket
  of its own (LEGION-664).

- An upload's kind is read from its bytes, never from the type its client declares: it is an
  `image` only when `http.DetectContentType` reads its bytes as a PNG, JPEG, GIF or WebP, the
  pictures a model is shown, and its `mime` is then the type its bytes are, whatever was declared
  (a JPEG declared `image/png` is stored and served as `image/jpeg`; a PNG declared
  `application/octet-stream` is an image). Any other upload but a `text/markdown` document is a
  `file` under its declared type, an SVG among them, so a client can no longer store arbitrary
  bytes as an image. This holds for an issue's, a project's and an agent's conversation's uploads
  alike. Stored artifacts keep their kind, so a new version of an `image` whose bytes are no
  picture is `400 ARTIFACT_KIND_MISMATCH` (LEGION-541).
- A new upload's slug keeps only the ASCII letters and digits of its lowercased name, each other
  run one dash (`café.png` is `caf-png`, `スクリーンショット.png` is `png`, a name with neither is
  `artifact`), for an issue's, a project's and an agent's conversation's uploads alike: every
  grammar that reads a slug back (the server's text references, the dashboard's routes, 0009's
  cleanup) is `[a-z0-9]+(-[a-z0-9]+)*`, so a slug holding another letter was stored but named
  nothing, and a picture pasted under such a name showed as a broken image (LEGION-541). Existing
  slugs are kept.
- `GET /api/v1/search` ranks each kind of content (issues, documents, asks, comments, messages) on
  a list of its own and merges the lists by reciprocal rank fusion (each hit scores 1/(60 + its
  position in its list), which is now its `rank`), so a page takes each kind's best in turn where
  it used to fill with long documents that repeat the query's words. Hits that score alike are
  ordered issue, document, ask, comment, message, and an issue whose key is the whole query heads
  the issue list (LEGION-386). The answer carries `total` (every match), `reachable` (the hits the
  pages can return, each kind's best `contracts.SearchKindDepth`, 100), `limit` and `offset`, and
  takes `offset` (0 or more; anything else is `400 INVALID_OFFSET`), so a query with more matches
  than one page says so and the rest can be paged to (LEGION-382).
  `TestSearchLatencyOnCorpus` no longer fails at an absolute 100 ms: production's `took_ms` already
  exceeds it, so `scripts/search-latency-compare.sh` runs it from a base checkout and this one on
  the same corpus copy and fails a median p95 more than 10% above the base's. `scripts/corpus-copy.sh`
  restores the newest nightly dump into a local Postgres, and `scripts/search-smoke.sh` prints any
  build's top hits for a list of queries.
- Event-log payloads served by `GET /api/v1/issues/{key}/events`, `GET /api/v1/artifacts/{id}/events`
  and the replay from `GET /api/v1/events` keep their stored numbers and PostgreSQL `jsonb` object
  order: `9007199254740993` stays that integer, `1.00` keeps its trailing zeros, and object keys come
  shorter first and, for keys of one length, in byte order. The JSON is compact, and Go's encoder
  still writes `<`, `>`, `&`, U+2028 and U+2029 inside strings as `\u` escapes, as it did before.
  Six ask payloads are decoded to add read-time fields and come back in Go's sorted-key, `float64`
  form: `ask.opened`, `ask.anchor_refreshed`, `ask.answered`, `ask.resolved`, `ask.edited` and
  `ask.handed_back`; `ask.follower_added` and `ask.follower_removed` keep the stored order. A live
  event on `GET /api/v1/events` keeps its producer's field order. The dashboard, the agent Dispatch
  tools and the Legion daemon read named fields from these payloads, and none compares payload bytes
  or relies on Go's map ordering or `float64` rounding.
- People sign in to Dispatch with Google Workspace through the shared sign-in pool (OpenID
  Connect authorization code against `DISPATCH_SIGNIN_ISSUER`, with `DISPATCH_SIGNIN_CLIENT_ID`,
  `DISPATCH_SIGNIN_CLIENT_SECRET` and `DISPATCH_SIGNIN_GROUP`, all four required by cookie
  identity) and are named by lowercase email everywhere: the email in their pool username, never
  the `email` claim. Only a member of `DISPATCH_SIGNIN_GROUP` signs in, and Dispatch confirms the
  membership with the pool's refresh token at least hourly, ending every session of a person the
  pool refuses or drops from the group; the event streams and the document websocket re-check the
  caller on every heartbeat. The session cookie's payload encodes the email so any address
  round-trips; a cookie of the earlier shape no longer verifies. Header identity is only for tests
  and local harnesses, requires `DISPATCH_IDENTITY_HEADER_TRUSTED=1`, and never shares a
  deployment with the sign-in pool. Everyone who has signed in is recorded in `people` (migration
  `0068`), which is the assignee picker's list and the only names an issue may be assigned to. The
  GitHub reads (`/api/github/rest/...`) go to GitHub as the GitHub App installation, GET only and
  only a pull request, an issue or a commit's check runs. `DISPATCH_ALLOWED_LOGINS` and
  `DISPATCH_APP_CLIENT_SECRET` are removed and refused at boot; GitHub OAuth sign-in, the per-user
  GitHub token table (`users`, dropped by `0068`) and the GraphQL proxy are gone.
- Dispatch stores each person's sign-in pool refresh token sealed under a key derived from
  `DISPATCH_SIGNING_KEY` (`people.refresh_token`, a `v1:` format; `cmd/dispatch/README.md`,
  Identity). A stored value that does not open counts as no refresh token: each read logs
  `dispatch: a stored refresh token did not open; the person signs in again` at WARN without the
  value, and the person signs in again. Every boot retires each refresh token stored in plain
  text, by `0068`'s release or by one of its tasks during a roll or after a rollback: it revokes
  the token at the pool's discovered `revocation_endpoint`, then clears it; a sign-in that replaces
  one retires it first. A token the boot cannot revoke stays for the next boot and is logged at
  ERROR with the email and the error, never the token; a sign-in that cannot revoke the token it
  replaces goes on and logs its own ERROR line, `dispatch: a sign-in replaced a refresh token the
  pool did not revoke; sign this person out at the pool`. A redirect from the revocation endpoint
  is a failure, never followed. The boot logs how many it retired and how many failed, and stops
  after 30 seconds without holding up the start. A database backup holds no refresh token the
  pool would accept only once a boot of this release has retired every plain-text token, and then
  only for the tokens still in `people` when it ran: one `0068`'s release removed without revoking
  (a sign-in that replaced it, or a logout) stays valid at the pool in any backup that holds it,
  so delete every backup taken between `0068`'s deploy and the post-roll restart, or sign out at
  the pool everyone whose `people.signed_in_at` is after `0068`'s deploy. After the last
  earlier-release task is gone, restart Dispatch once, then check that boot logged `failed=0`, no
  sign-in-replaced line appeared, and
  `select count(*) from people where refresh_token is not null and refresh_token not like 'v1:%'`
  answers `0` (`cmd/dispatch/README.md`). Rotating `DISPATCH_SIGNING_KEY` leaves Dispatch unable
  to open any stored refresh token, so everyone signs in again, but revokes none at the pool:
  after a key leak, sign people out at the pool.
- A blank approval-request `summary` is refused (`400 SUMMARY_INPUT`) with text that asks for what
  the human is approving, rather than for what the version proposes that the human has not agreed
  to, and the advice in `409 APPROVAL_WAITS_ON_HUMAN` and in an approval ask's `409 ASK_KIND_FIXED`
  says to hand the request back only once the human has agreed to every point in the document
  (LEGION-475).
- A Dispatch approval request follows its document's versions instead of being retracted and
  reopened on every edit (LEGION-470). A version write moves the open request to the new version
  (`ask.edited`), keeping its thread and summary, and leaves it Waiting on agents until its agent
  calls `POST /api/v1/artifacts/{id}/approval-requests` again. Only the first move since the
  request was opened or handed back wakes its asker and followers, even when a reply in its thread
  had already left it waiting on its agent; each later move, while `requested_version` is already
  below the version it named, is recorded quiet (`quiet: true` on its `ask.edited`,
  `notify: false`, no follower route), as a human's unnamed version is, so a person typing in a
  document with a request open sends one delivery, not one per settled version. While the request
  waits on its agent (moved, or answered in its thread), that call hands it back to the human with
  the new event `ask.handed_back`, rewording it first when the summary is new (`ask.edited`); a
  hand-back that rewords nothing leaves the question and `edited_at` alone, so an answer the human
  had started is still saved. While the request already waits on the human, the same summary is a
  repeat that writes nothing and a different one is refused `409 APPROVAL_WAITS_ON_HUMAN`, since an
  approval request carries nothing new. The route answers 201 when it wrote anything and 200 when
  the request already stood as asked. A summary's limit is counted against a ten-digit version, so
  no version move takes the question past the ask cap. `ask.approval` gains `requested_version`, the
  version last handed to the human, and a document's `approval` gains `waiting_on` while it is
  `awaiting`. Every route that writes a comment (both comment routes and the delivery callback
  `POST /api/v1/comments/{id}/reply`) answers `ask_waiting_on`, the value the comment's event
  already carries, beside the comment on a reply to an open ask; a replayed delivery callback,
  which writes nothing, answers the stored reply alone. Migration `0064`
  backfills `requested_version` and adds `asks.handed_back_reply_id`, `0065` makes
  `comments.created_at` default to `clock_timestamp()` so an ask's newest reply is the one that
  committed last, and `0066` adds the column's foreign key to `comments` under a 500 ms
  `lock_timeout` of its own; each census answers `0`. The pre-deploy census judges a pending
  migration's lock holders against the `lock_timeout` the migration itself sets, so an autovacuum
  on `asks` or `comments` refuses `0066` where `deadlock_timeout` is not shorter than its 500 ms.
- A markdown document uploaded as an artifact is at most 1 MiB, the bound an issue's spec and
  every edit already have; other artifacts keep the 25 MiB limit. The dashboard shows the
  server's message for a refused upload (LEGION-465).
- The stream stores an envelope under a MsgId, and so recognises its repeat, when its dedupe key was
  minted once for its message: the listener's own `publish.<id>` and `agent.<session>.<id>`, and the
  same around the shared transport's UUID idempotency key (`contracts.MintedDedupeKeyPattern`,
  generated from `MINTED_DEDUPE_KEY_PATTERN` in `packages/contracts`). Only a re-send of that
  message repeats such a key: the transport's retry of a send whose answer was lost, which now
  answers `duplicate: true` and is stored once, and the Legion daemon's copy of a role-lane notice
  (LEGION-108). Before, only Dispatch and webhook delivery-id keys earned a MsgId. The same rule,
  `dedupeKeyNamesItsEvent`, now decides what a core-NATS host drops.
- The Dispatch dashboard no longer offers a same-mode **Retry** for a targeted message or comment
  mention its session answered with an error (a BTW whose side turn failed, a frame its host
  refused). The stream already stored that attempt's frame under the Retry's key, so a session the
  listener pushes to from the stream was never handed the Retry, and the attempt then read
  "Delivered by an earlier attempt". The card now says the session answered with an error and
  points at its mode-change actions; the mention list points at a new comment. Sending that Retry
  under a new key is LEGION-431. On a closed issue the mention list, like the card, no longer
  promises "Retry won't deliver it twice" beside a failure it offers no Retry for.
- `POST /v1/messages/publish` refuses a `dedupe_key` on a `source: "dispatch"` envelope with a 400
  naming `dedupe_key`. Every host drops a repeat of a Dispatch key, and Dispatch's outbox numbers
  its keys in sequence (`dispatch-<event id>`), so any holder of the listener bearer could publish
  `dispatch-<next id>` on a topic someone follows and make that host drop the real event when it
  arrived. Dispatch never set one there: its outbox publishes to the bus directly, and its sends
  go through `/v1/messages/send`, whose key the listener makes.
- Dispatch's conversation view (`/agents/<id>/live`) sends as Send by default wherever the
  session advertises steer, and as Aside otherwise, and names the modes Send, Aside and BTW. A
  person's message that an Oh My Pi session took as its own user turn shows once, where the
  session took it, still naming its author, and only when the streamed text is the stored one;
  on a host without `pi.sendUserInput`, one the session took between a run's last queue or aside
  poll and its `agent_end` reaches the stream untagged and shows twice
  (`packages/pi-envoy/AGENTS.md`). The Agents page shows an attempt the session accepted as
  conversation, with no retry, even after a later `failed`; every other attempt keeps its states
  (LEGION-394). Every label the dashboard composes for a mode now uses the composer's names (Send,
  Aside, BTW), the card's mode-change buttons included ("Use BTW instead", "Use Send instead",
  which read "Send as BTW instead" and "Send normally instead"). A delivery error or broadcast
  exclusion reason Dispatch stored keeps its wire name (`does not advertise steer`), as agents and
  scripts read it (LEGION-394).
- `GET /api/v1/issues` pages with `limit` (1–250) and `offset` (0 or more; `offset` alone pages
  50), answering `{issues, total, limit, offset}` with `total` counting every issue the filters
  match; without either parameter it answers the whole listing as an array, as before. It used to
  ignore both, and `cursor`, answering 200 with every row (on production, `?project=LEGSMOKE` with
  `limit=5`, `offset=5`, `cursor=abc` or `limit=999` each answered the whole project), which a caller
  that asked for a page cannot tell from one (LEGION-406). A repeated, blank, non-integer or
  out-of-range `limit` or `offset` is `400 INVALID_QUERY` naming the parameter, and so is `cursor`,
  which the listing does not page with. The page is cut after every filter, `route_status`
  included. The listing's order now ends on the issue key. Status, rank and creation time can tie
  (every project's first issue has rank `U`, and nothing makes a rank unique), and Postgres
  returned such ties in an order of its own: in a project whose issues share one creation time and
  alternate between two ranks, the old order listed one rank as `S8-2 S8-6 S8-4 S8-8`. A walk of
  the pages is exact only while the listing does not change. The Stage 4b live proof no longer
  sends `limit=200` (its preflight) or `limit=250` (its daily-report check); each would now answer
  a page its `jq` cannot read.
- The CI summary loop publishes a `pr.<n>.checks` settlement for every commit of a pull request
  whose checks settle, not only its current head, carrying the commit's `sha` as before. A head
  pushed with GitHub's `skip-checks` trailer runs no CI, so the commit it replaced settles for it
  after it lands (LEGION-208). Consumers decide which commit a settlement stands for: the
  TypeScript daemon still takes only its head's, and the Go daemon takes a code head's for the
  handoff-only heads after it. The listener no longer records each pull request's head (the
  `head.*` records in the CI bucket); a record an earlier listener wrote is skipped until its TTL.
- Every write of a CI record stamps it `schema: 1`. A record without it was last written by a
  listener that settled only heads, and it settles only when its last event is between the
  debounce and the debounce plus five minutes ago, so a head that finished during the handover
  still settles and the backlog of commits that listener never settled does not: on 2026-09-28
  production held 1,442 such records (595 `pr.<n>.checks` subjects, up to 168 hours old), which
  the first start would otherwise have published at once. Those records stay unsettled, without
  `schema`, until the bucket's seven-day TTL expires them; the `/metrics` gauge
  `envoy_ci_legacy_records_held` carries how many the last tick held back, and the first tick that
  holds any logs the count, a floor, once at INFO. An observation that changes the record
  stamps it and the commit settles as usual; a stamped record settles however long it waited,
  across a restart too. A rolled-back listener ignores the field. A listener built with #1526 but
  without this change publishes the whole backlog, so every head-gated listener moves straight to
  a build carrying this change.
- `GET /api/v1/search` refuses a `q` over 1,000 UTF-16 units, counted after trimming, with the
  ordinary `400 CAP_EXCEEDED` and a sentence saying what to send instead (LEGION-386), and a
  `project` that is not a project key with `400 INVALID_PROJECT`, where a lowercased key used to
  answer an empty result. A mistyped key that still has a key's shape, such as `LEGOIN`, is still
  searched and still answers empty. An empty `project` still searches every project. Both ride
  in the URL, and the load balancer in front of production Dispatch answers a URL longer than it
  accepts with a bare `414` before the server sees the request. So deploying this names the rule
  only to a request that still fits; it cannot answer the 26,637-character search that prompted
  LEGION-386. Only an updated `@sjawhar/pi-legion-envoy` in a session's profile (or the OpenCode
  or Claude Code plugin built from the same executor) keeps that URL from being sent at all,
  because its `dispatch_search` refuses the same rules before any request.

- A Markdown document now nests at most 100 blocks, and a document tree with a node more than 1,000 levels below the document, a node attribute value nesting more than 100 arrays and objects, or a mark attribute value nesting more than 99, is outside the Proof schema (LEGION-465, LEGION-535). The bounds sit where every read serves the tree: past about 5,000 levels the document token is JSON that `encoding/json` will not write from Go 1.27 or read in any version, and `GET /blocks`, which hashes each block's subtree apart, does work growing with the square of the depth. A live tree past either tree bound is treated as any other tree outside the schema: settlement writes no version, its reads and edits answer `409 DOC_SCHEMA` naming the repair, the document websocket refuses it, and an upload of replacement markdown repairs it (LEGION-469). A textblock's inline markdown nests at most 100 marks inside one another - emphasis, strong, strikethrough, links, images and code - and deeper content is refused naming the line. An accepted suggestion whose own markdown nests within 100 blocks but lands deep enough that the document would nest past them is refused as `400 INVALID_OP` on `replace_with`, naming how many blocks the result nests (LEGION-465).

### Fixed

- A closed issue can give up an external link (a write whose links are a subset of what it already
  holds) so the link can move to the issue that replaced it; adding a new link to a closed issue
  still refuses with `ISSUE_CLOSED`, as every other closed-issue content write does.

- A bus client whose NATS connection closes while a publish is in flight now replaces it once,
  and `Close` and `Drain` close every connection it dialled (LEGION-765). The recovery the close
  starts and the publish each dialled a replacement, and both were installed in turn, so the
  first was left connected and was never closed. Once its server went away it reconnected forever,
  once a second, in the listener, Dispatch or any other process holding a `bus.Client`. Now the
  second to finish closes its own connection, logs `envoy nats discarded a second replacement
  connection`, and serves with the first. Its close logs no `envoy nats disconnected` and starts
  no recovery. No message is lost or sent twice: the discarded connection never carried one.

- Dispatch's job intake now receives job events (LEGION-690). It shared Dispatch's publisher
  client with the delivery intake, and a `bus.Client` held one JetStream subscription, so
  whichever intake bound second unsubscribed the other, whose durable then sat unbound with its
  messages pending. Each intake now connects on its own, and a `bus.Client` refuses a second
  subscription on another subject (`bus.ErrSubscriptionHeld`) rather than replacing the first.
  With the GitHub App configured, Dispatch holds four NATS connections where it held two, named
  `dispatch`, `dispatch-agent-stream`, `dispatch-intake` and `dispatch-job-intake` on the server,
  and exits at boot when it cannot open them, so a NATS user or account with a connection limit
  must allow four; without the App it holds two, as before. Each bus client's lifecycle log lines
  now carry its name. On its first start the job intake's durable is handed every job event it has
  not acknowledged, up to the stream's 72-hour retention, and reads at most eight runs' jobs from
  GitHub at once while it drains, before it settles at nothing pending.

- The `GET /ws/delivery` socket gives itself a read buffer, so a page that stops answering its
  pings loses the socket with no close frame (1006 to the page) under any gorilla/websocket
  (LEGION-690). Without one, a build linking a gorilla that keeps net/http's hijacked reader (every
  workspace build, through `packages/daemon`'s client-go) could close it with 1008, the code for a
  caller that no longer resolves, depending on a race. The published image, built in module mode
  with gorilla v1.5.3, never did. The page reconnects with backoff after either close.

- The delivery live hub arms its one timer for the moment the next view is due, rather than for a
  wait counted from the moment its worker read the clock (LEGION-690). A wait put the timer late by
  however long the worker took between that read and the arm, and a test that moved its clock
  forward in that gap saw no timer fire at all, so its Pipeline view never moved past midnight. A
  timer armed for a moment the clock has already passed fires at once.

- The delivery live hub no longer re-sends a run in which nothing changed (LEGION-690). Each item it
  sends is revised by a hash of its JSON, and several store reads answered rows tied on their order
  in whatever order Postgres's plan read them: a run's jobs (no order at all), the pull requests a
  run shipped and the waiting list (merged in one second), and runs started in one instant. The
  plan alternates between a sequential scan (the table's row order, where a reconcile pass's
  unchanged rewrite moves a row last) and an index scan, so a recompute answered a run's
  `failed_jobs`, `root_failing_job`, `production` or `prs` in another order and the hub sent the
  run again, whole: in production deltas of 54 to 61 runs, about 110 KB each, whose one change was
  the order of their `prs`. A run's jobs now come back by when each started, a job that never
  started last, then by name (`ListRunJobs`, `ListRunJobsForRuns`, sorted in Go per run: an
  `order by` sorts the whole 28-day read on disk, 82-93 ms against 43-52 ms for the unordered read
  and 3 ms for the sort), the order `GET /api/v1/delivery/runs/{id}` already listed them in. Pull
  requests merged together come back by id (`owner/repo#N` in byte order, the page's own order) and
  runs started together by run id. A run's root failing job and its production job, among jobs
  finishing together, are the first by name, and a run whose production jobs all failed falls back
  to the first by start rather than the first listed. The Pipeline page's failing jobs name, as a
  job's last failing run among runs that completed together, the one with the higher run id rather
  than whichever was listed last.

- Two production deploys whose jobs complete in the same second are ordered by run id, and the
  lower run id ships a pull request both could, so the live Delivery hub's sliding window no longer
  swaps a pull request's `deploy_run` and a run's `prs` with no stored row changed (LEGION-690).

- The approval carry holds the version a human approved to the document's asks rows as it holds
  the version his answer writes (LEGION-752). A block of the new version counts as unchanged since
  his approval only while the approved version holds a block under the same id whose stored
  attributes and, for a decided row, wording are what settlement's own writer leaves from that
  block's row - the whole server-owned set, not `state`, `answered_by` and `answered_at` alone -
  so an approved version stored inside the settle window, showing a selection, an answer, a copy's
  provenance or a question other than its row's on a decided block, no longer compares as
  unchanged against the row-true version his later answer writes: that block is a change, which
  must be his own answer since his approval, and his answer to another block carries nothing
  (`stale`, as before the carry existed). The same sequence over an approved version whose block
  is as its row has it carries, as before. The approved version's blocks are read as the new
  version's are (`askNodes`); a stored version's read-back repairs a repeated block id before the
  carry reads it, so a stored version that held an id twice shows a block the new version never
  had and carried nothing before this change either.

- A picture written into a spec or document as `![name](dispatch://KEY/artifact/<slug>@vN)`, the
  line a pasted picture is written as, now shows in the document editor and in a past version,
  drawn from the version's bytes route; before, the editor's `<img>` carried the `dispatch://`
  address as its source, which no browser fetches, so the line showed nothing. A version the
  browser cannot draw (a markdown document, another file, a missing version), or a source that
  pins no version, shows a link to what it names instead, its text the reference's short form
  (`CORE-1 notes-md`). A picture from another website is drawn as before (LEGION-695).

- A listener's shutdown drain no longer waits out 5 s of its 10 s budget when its NATS connection
  closes while nats.go drains it. nats.go v1.50.0 marks a connection closed during its drain as
  draining again and waits 5 s for a reply on the socket it closed, and the drain read the
  connection's state, so it could miss the close; it now waits for the connection's closed event.
  The listener's own reconnect callback was one such close: nats.go can run the callback for a
  reconnect that came before the drain once the drain is under way, and it closed the connection
  there, cutting off the deliveries still in their handlers. A reconnect callback now closes the
  connection only for a reconnect that came after the drain began.
- A person's new comment on a document reaches the agents that wrote it, each once, on its own
  `notifications.agent.<session_id>` topic, whatever the issue's route: a comment on a passage of
  an issue's document or a project document, a comment on a whole project document, a suggestion,
  and an edit of any of these. The writers are the session that created the document and every
  session one of its versions credits, which includes an agent whose anchored comment credited it
  on a new version. Before, a new comment found no author to route to, so the agent heard of it
  only by subscribing to the issue's or document's own topic (LEGION-681). A document only people
  wrote sends nothing new, since a person has no topic. A reply still reaches its thread's root
  author and its parent's author alone, an ask reply its followers, and a comment on an issue's
  conversation, which names no document, no author. An agent's comment still notifies nobody, so a
  session commenting on a document it wrote is not told of its own comment. A writer the comment
  @-mentions hears it once, from the mention: a new comment's `suppressed_authors` now names every
  session a mention resolved to.
- `scripts/dev-postgres.sh` reuses its `dispatch-pg` container rather than failing on the name: it
  starts a stopped one and serves a running one, when the container runs the pinned pgvector
  image. It refuses one on any other image, such as a `postgres:16` an older version of the script
  created, whose missing `vector` extension failed Dispatch's migrations at `0072`. The refusal
  names both images and prints the commands that move the container's data volume onto the pinned
  image, read from `docker inspect` and quoted so a bind mount's path with a space pastes whole;
  the script itself stops, removes and recreates nothing. `scripts/dev-broker.sh` boots Postgres
  through it, so it refuses that container too, before it creates a database.
- A document whose search vector would pass Postgres's limit on one vector (1,048,575 bytes of lexemes and positions) can be versioned. Text of words no two alike passes it well inside a document's 1 MiB, at about 700 KB of `w000001 w000002 …` or 475 KB of UUIDs, and every write of such text failed with `string is too long for tsvector`: the document's version, so every settlement of it, its upload, an issue titled with it and that title's duplicate check, and an ask block holding it. Migrations `0087`–`0088` build every search vector with `search_vector`, which indexes the whole text where its vector fits, exactly as before, and otherwise the longest of its first half, quarter, eighth, … that fits, cut between two words, so keyword search finds such a text by the whole words that open it and by no part of a word the cut would split. A body with no whitespace before its cut contributes none of its own words; an issue's key is always indexed whole (LEGION-505).
- An issue title is at most 1,000 characters (`contracts.IssueTitleMax`, UTF-16 units after the trim), on creation and on a retitle; a longer one is `400 CAP_EXCEEDED`, named as the comment, message and ask caps name theirs, before the duplicate check reads the project's titles. Titles had no cap, so one stored title could be as long as a request allowed: every later creation in its project read it in the duplicate check, and the `409 POSSIBLE_DUPLICATE` message quoted it whole. Beside 2,000 production titles, one stored 800 KB title raised the check from about 0.1 s to 0.56 s. A new title of 20,000 distinct words (10,000 did not) beside an issue whose title shared one of them overflowed Postgres's default 2 MB stack in the duplicate check's snippet (`stack depth limit exceeded`, `500`); a title at the cap holds at most 501 lexemes. Production's longest title is 326 characters. The dashboard's create dialog and issue-header title field stop at the cap, and a retitle Dispatch refuses shows Dispatch's reason in the header instead of the generic failure line (LEGION-505).
- The duplicate-title check no longer parses every title in the project on each creation. It read each stored title's lexemes by building them from the title as it ran, several times over for a title that shared a word with the new one, so its cost grew with every title in the project: beside 2,000 titles at the 1,000-character cap, an ordinary creation's check took 2.1–2.5 s and one at the cap 7.6 s. Migrations `0088`–`0089` store each title's own lexemes in `issues.title_lexemes`, which the issues trigger fills on every insert and retitle and `0089` fills for every issue stored before, under a table lock that no reparent deadlocks against; the check reads them, so a creation parses one title. Beside those 2,000 titles the check takes 56–95 ms, and beside 2,000 production titles 15–19 ms where it took 120–154 ms (Postgres 16, load 60–80). It answers the same candidates, in the same order. A candidate's snippet marks the words its title shares with the new title: with a parent, a word that is both the parent's and a part of a hyphenated word of the new title is no longer marked, where the snippet marked it beside parent `Legion` in `launch window legion` for the new title `legion-resolve launch window` (LEGION-505).
- The delivery timeline and measures no longer log `a successfully concluded run has no job
  matching the configured production_job_name` for a deploy run that skipped its production job.
  GitHub lists a skipped call of a reusable workflow under the calling job's name alone, so a
  dev-only run lists `production-apply` where a deploy lists `production-apply / production-apply`.
  The skip is taken as the reason only while another run the same read judges holds a job by the
  configured name, so a typo whose first part names a skipped job (`staging-apply /
  production-apply`) still warns; a window with no production deploy in it warns for each of its
  successful runs. Such a run still counts as no deploy. Every warning is now logged once per run
  and configured job name for the life of the process rather than on every read: one dev-only run
  had been logging the line twice every three minutes (LEGION-567).
- A document uploaded or seeded with another document's ask blocks, under their block ids and with
  the same question, options, `multiple` and urgency, on the same issue (or the same project for a
  project document) opens no ask for them: each copied block shows the state and answer of the ask
  it was copied from, read-only, and names it and its document in the server-owned `copied_from`
  and `copied_from_document`, where settlement had opened a new ask in a person's Inbox for every
  copied block, answered and resolved ones included. Of several matching asks the earliest asked
  is the source, whichever wording it matched on. A block matching only a source's earlier wording
  is a copy of it while the source is open, or when the source's answer or resolution was given
  while it asked that wording, ordered by the events' commit order; one answered or resolved after
  the rewording opens its own ask, so no copy shows a decision on a question it does not ask.
  Answering or resolving the source reaches its copies a settlement later. Each block settlement
  takes for a copy, and each change of the state a copy shows, is logged once (`dispatch: settled
  an ask block as a copy of another document's ask`, naming the copy, its block, the source ask,
  its document and its state, never the question), since such a block opens no ask and reaches no
  one's Inbox.
  `dispatch request-approval`'s refusal and the dashboard's decision card name the source, its
  document and its state from those attributes. A copy whose text is changed, or an id no ask of
  the owner indexes, still opens one, and so do a source's copies once its block
  leaves its document: settlement of the source retracts its ask and settles every copy, the first
  opens one ask, credited to whoever wrote the block into it, and the others name that ask, on a
  project's documents as on an issue's. Migration `0091_artifacts_project_documents` adds the
  partial index a project document's lookups read. An answered or resolved ask's wording is fixed:
  a document edit or an upload that changes its block's question, options, `multiple` or urgency
  no longer rewords the ask and leaves its answer beside a question nobody answered; settlement
  writes the decided wording back into the block and logs it, as `PATCH /api/v1/asks/{id}` already
  refused such an edit (`ASK_NOT_OPEN`). Each ask an earlier release reworded after its decision
  gets back the wording that decision was given on, in code and not by a migration: every boot
  restores them all in one statement and logs each ask, its document and its question, and each
  document's settlement restores its own before it writes the decided wording back into the block,
  which also undoes a rewording an earlier release's task writes during a rolling deploy. The boot
  settles each restored ask's own document, whose block still asks the rewording until that
  settlement writes the decided wording back. Either restoration settles the restored asks' copies
  again: a copy that settled while its source's row held the rewording showed the decision on the
  reworded question, and now opens its own ask (LEGION-651).
- A document opens in the editor however many documents the process has touched: the 1,000-room
  cap counts ygo's live rooms, and a document's in-memory state is released once its room goes and
  nothing still holds it. Before, every document opened since a restart kept its state and counted
  against the cap, so editors were refused with 503 after about 1,000 (LEGION-513).
- A document's pending authors now survive room release, process restart and overlapping Dispatch
  tasks in `doc_pending_authors` (migration `0084`). A browser update is first an in-flight,
  room-local credit (F); its append moves an unconsumed credit to the durable record (R) under the
  document lock. A joined write records its authors in R in its content transaction. A version
  reads R under that lock and may capture F only from its own room; after its transaction commits,
  it deletes the R rows it listed and consumes the F credits it listed. The scoped rule means a
  task can list another task's durable R records but never that task's F, while the same task can
  consume F before its queued append can re-record an author. Each author is consequently pending
  in F or R, or listed on one committed version, rather than in more than one of them. A settlement
  that writes no version leaves R intact, and an upload that writes a replacement clears all R and
  only the F credits present at its last room read. The document room can therefore go idle without
  retaining author state or losing the authors a later version, ask or event must name
  (LEGION-513).
- `GET /api/v1/asks/open` and `GET /api/v1/me/answers` give an issue ask's `ref` as its item
  route, `/issues/<KEY>/asks/<id>`, where they gave `/issues/<KEY>?ask=<id>`, which the bare
  issue page does not read, so following it landed on the issue and not the ask. A document ask's
  `ref` is unchanged (LEGION-622).
- The secrets broker no longer logs `agent secret policy load failed; previous policy kept` for a
  periodic reread of the namespace that its own shutdown cut short: `policy.NewCurrent` logs a
  failed reload only while its context is live, so the deployment's alarm on that line no longer
  counts a shutdown as a failed load. A reload that fails while the broker runs logs exactly as
  before.
- Agent bearer tokens can now list repository-to-project mappings and architecture sources, and set
  or remove repository mappings, architecture sources, and delivery settings. Settings writes record
  the bearer-supplied session actor, as other agent-authenticated writes do.
- Every read of an artifact's `project_key` tolerates a null: `scanArtifact` (every artifact read
  by id, ref key, owner or name, and both anchor locks), an ask's anchor artifact, a comment
  event's and an anchor refresh's payload, a suggestion's project, a document write's owner lock,
  and search's owner project (a nil dereference there, not a scan error) answer an empty project
  instead of `cannot scan NULL into *string`; the reference graph gives such an artifact, and an
  item on it, no address rather than `dispatch:///artifact/<slug>`. Landed on `main` ahead of
  migration `0071_agent_artifacts`, which gives an artifact a third owner with no project
  (LEGION-541), so a binary from before that migration, serving beside it during its rollout or
  after a revert, reads those rows.
- A published edit's check of whether a browser's concurrent change removed its text (`lost_ops`, and an accepted suggestion's `lost`) walked the room's live tree without its lock while the room's browsers wrote it, so it could read a keystroke halfway. Every read of a resident room outside a write now reads the room as of one moment under its lock: the replica the room's update observer keeps, brought up to date, or, while the observer or another read holds that replica, a copy as before. A read only tries the replica, so reads never queue behind each other or ahead of a keystroke's observer, but a keystroke can still wait for one read already walking it (about 100 ms on a 524 KiB document). A read through the replica takes about a third of a copy's time (86 ms rather than 283 ms for a 524 KiB document's tree). A tree a read returns shares nothing with the document it was read from, so editing it changes no later read (LEGION-499).
- A deploy with someone on the Dispatch dashboard no longer spends the whole shutdown waiting and
  then leaves the documents it owed unsettled (LEGION-501). An open event stream or agent
  conversation stream never went idle, so `http.Server.Shutdown` held for its full 5 s, and the
  document service then shut down on that expired deadline and logged `document settlement
  unconfirmed at shutdown`. Both streams now end at SIGTERM, which the dashboard reconnects from,
  so HTTP shutdown waits only for the requests in flight. Those requests drain first, with no
  deadline of their own; the document service starts once they are done, or 11 s after the signal
  with some still running, and gets 14 s of its own, so it finishes 5 s inside the 30 s stop timeout
  ECS and the compose file give Dispatch. Each loaded document drains, reads what it owes and
  settles in a worker of its own within 9 s of that budget, so a burst of writes still landing
  after the worker started is waited for too, not only the ones already queued, and a settlement
  still committing once that 9 s ends keeps the remaining 5 s to finish rather than ending in an
  unconfirmed error: a document whose update is slow to store, or whose editor
  keeps typing, leaves only its own settlement to the next process. A spec with its tab open is
  settled while its room is still loaded, and its editors are disconnected only after that, so the
  edit they made is versioned before the process exits instead of when someone next opens the spec;
  a settlement that has to write into the room (stamping a block id, restoring an ask block's state)
  is still left to the next process, now with a WARN rather than an ERROR. The database pool closes
  once every request has answered, with nothing but the runtime's kill bounding that wait while the
  database answers, so a write waiting on a lock commits and is answered if it finishes before the
  kill. A database that has stopped answering no longer holds the process until its runtime kills
  it: once the document service has finished and three health probes in a row have failed with no
  connection returned meanwhile, Dispatch exits without the connections waiting on it (`dispatch:
  exit with database connections still in use once the database stopped answering`). The compose
  file sets `stop_grace_period: 30s`.
- A document's stored update log kept every byte any write had inserted, and every cold load of it built all of it: Dispatch merged the stored updates whole, and a merge keeps the content of deleted items. Five hundred 2,000-character replies to one anchored comment, each projecting the thread's margin record again, left 257 MB stored under a 3 KB document, and a cold one-word edit of it then took 1,842 MiB, past the 1,024 MiB task. A load now applies the stored updates one at a time to a document that collects garbage and returns what it holds, so it costs the live document and one update; and compaction, which runs as a room closes, as the server shuts down and daily, folds the whole log into that state rather than keeping the newest 500 updates beside a merge of the rest. An update a load's document parks for a dependency the log lacks is merged back into that state. A log the fold cannot apply, or a state that does not read back as the document that made it, is not used: the load merges the stored updates whole as before, and compaction leaves them as stored. After compaction, `doc_updates` and any backup of it hold no text a write deleted, so text deleted before a version captured it is gone (Sami's decision, LEGION-496). `doc_updates` is snapshotted once, and the snapshot kept 90 days, right before the first deploy that carries this.
- A document edit that repairs an ask a browser left unreadable now reports it in
  `decision_blocks_added`, matching the open ask its next settlement creates. An edit keeps that
  count as count-only advice when the bounded issue-advice query fails (LEGION-470).
- A NUL character (U+0000) in caller text no longer answers `500 INTERNAL`: PostgreSQL's text and jsonb cannot hold one, so every Dispatch route that stored such text (issue titles, specs and labels, comments, messages, broadcasts, asks and their answers, document uploads, edits and versions, a bearer's `actor`, a session id in a path) failed at the database. The API's input layer refuses it once instead, before anything is written: `400 NUL_CHARACTER` naming where it stands and its position, as in `options[1].label holds a NUL character (U+0000) at character 2, which Dispatch cannot store`; a NUL in a name the caller wrote is shown as `\u0000`, so no refusal sends one back. It reads every string and member name of a JSON body, a multipart upload's fields and markdown file, the body a credential decision or machine-login lookup relays to the secrets broker, and every route's path and query parameters, so a read such as `GET /api/v1/issues?label=` refuses one too. The document websocket refuses, before the upgrade, a bearer whose `X-Dispatch-Actor` JSON spells one in its session id or origin: that actor became the author of the versions the connection's edits made, so every settlement of the document failed and the third failure in a row failed its room. The check covers every parameter, field and member a request carries, so these requests main accepted are now refused: a retype whose typed attribute holds a NUL, an insert whose markdown holds a raw NUL inside a typed block's quoted attribute (written as the escape `\x00`, the same value is still taken), a query parameter or multipart field no route reads holding a NUL or a byte that is not UTF-8, and a websocket `X-Dispatch-Actor` whose unread member spells a NUL (LEGION-507).
- A byte that is not UTF-8 in a path or query parameter (`%FF`), in a multipart upload's field or in a binary file part's `Content-Type`, which the upload stores as the artifact's type, no longer answers `500 INTERNAL`, and one in an uploaded markdown file no longer panics the server, which dropped the connection: PostgreSQL's text cannot hold such a byte, and the document writer refuses one by panicking. The input layer refuses it where it refuses a NUL, `400 INVALID_UTF8` naming the field, the byte and its position. A JSON body cannot carry one, since decoding writes each such byte as U+FFFD (LEGION-507).
- A NUL character a browser edit puts in a document no longer stops the document from settling. The Yjs update reaches the live document past every route's check, and the version settlement rendered from it, an ask block's question and an anchored comment's or ask's quote each failed at PostgreSQL, so settlement failed every time it ran; with the character in the document's text or under an anchor, every agent edit and named version of the document answered `500` too, until someone deleted it in the editor. Dispatch now writes the character as U+FFFD, which is what CommonMark reads it as, in the markdown it renders and serves and in the ask and anchor text it takes from the document, and matches a quote, a `# Title` quote and a `heading:` anchor against that text, so one copied from `GET /text` finds its target; a typed block's other attributes keep it, written quoted as the escape `\x00` that reads back as U+0000 (LEGION-507).
- A block id holding a NUL, which a hand-built client can set on a document block, no longer stops the document from settling. An ask row or anchor could not store it, so settlement failed every time it ran, a comment or ask anchored inside that block before settlement answered `500`, and while it was a typed block's id `GET /text` and `GET /blocks` served it and naming a version answered `500`. Settlement now mints a fresh id for it, as it mints one for a missing id; until then an anchor taken in the block is pinned to no block, and every read and named version writes the id with U+FFFD in place of the NUL. Every other id stays as the browser set it, such as `q:1` from a pasted `:::ask{#q:1}`, which the browser editor keeps and an ask row stores (LEGION-507).
- An artifact upload whose multipart body the parser cannot read, such as one whose part header holds a control character, is `400 ARTIFACT_INPUT` with the parser's reason. It used to be answered `413 CAP_EXCEEDED "artifact blob exceeds 25 MB"`, as was every multipart parse failure; only a body past the upload's size limit, or form fields too large to read, is still 413, and a file part the server cannot spool to its temporary directory is `500 INTERNAL`, logged, since the failure is the server's (LEGION-507).
- The architecture sync no longer answers `500` and records nothing when a component's front matter spells a NUL through a YAML escape, in a value (`title: "Ser\0ver"`) or an unknown key (`"\0": value`): the parser checked the file's bytes, which are ASCII there, and the projection or the failure-recording update then failed at PostgreSQL with no `last_error` and no `architecture.sync_failed` event. The parser checks each decoded front-matter value and names its field; before `last_error` and the event payload are stored, every failure message passes through `text.StorableReplacement`, which writes a NUL or a byte that is not UTF-8 as U+FFFD. Any projection PostgreSQL refuses is recorded on the row like a model problem, so `POST /api/v1/projects/{key}/architecture-source/sync` answers 200 with `last_error` and the previous projection stays (LEGION-527).
- A Dispatch page whose HTML and assets came from different builds stayed blank: during a rolling deploy the load balancer sent a browser's HTML to one task and its hashed entry chunk to the other, which never had it and answered 404, and nothing reloaded the page, since the lazy-chunk recovery runs only once the entry has. The page now reloads itself once when one of its own build's scripts (same origin, under `/assets/`) fails to load, from the same once-per-session budget as the lazy-chunk recovery, never while offline or while the page is being left (from `beforeunload` or `pagehide` until `pageshow`); another origin's script, such as an extension's, never triggers it. Every page the server serves (`index.html` and the shell for a browser route) now carries `Cache-Control: no-cache` and an `ETag` of its bytes in place of `Last-Modified`: a browser could run a heuristically fresh page from an older build after any deploy, and a revalidation by file time could keep another build's page, since a task holding an older file (a rollback, or the other side of the overlap) answered it 304. Every file under `/assets` carries `public, max-age=31536000, immutable`, and `GET /index.html` answers the page rather than redirecting to `/` (LEGION-521).
- A listener whose session cache had not yet seen a role holder another listener registered a moment ago released the holder's fresh claim as lapsed: a lookup (`GET /v1/roles/<role>`, a role publish) or a role delivery read the claim from the role bucket and the holder from a cache that trails the session bucket, so during a rolling deploy the old task could delete the claim the replacement had just accepted, and the role reached nobody until its holder claimed it again. A soft claim could take such a holder's role, and the role reaper end its claim, the same way. Each now reads the holder from the session bucket itself before it takes anything from it (LEGION-456).
- One MiB of `>` formed 1,048,576 nested quotes inside the document cap and eventually ended the process in a stack overflow while its tree was validated. Dispatch now refuses the document before building that tree (LEGION-465).
- Reading a textblock's inline markdown took one stack frame per nested mark, so the stack and memory it needed grew with the nesting the caller wrote: one 1 MiB upload of 262,140 nested strong marks read with no error but peaked at about 0.9 GB of memory. The inline bound above is checked before any walk of those marks that recurses, and goldmark's own walk through a link's label, which enters every image the label holds, meets each image held to the bound as it is made (LEGION-465).
- A table whose rows hold an escaped pipe in a code span parsed in time quadratic in its size: goldmark's table transformer checked every code span's text against every escaped pipe in the document, and 1 MiB of such rows took over two minutes. Dispatch takes the backslash out of those pipes itself, in one pass, and 1 MiB parses in about two seconds (LEGION-465).
- An id outside nats.go's key alphabet (`ses:bad`) failed whatever met it in the interest and role buckets, since nats.go refuses such a key on every read, write and delete and the stores took that refusal for a failure. A role claim over such a holder, which an earlier build's bare-string claim or a direct bucket write can leave in the role bucket, wrote the claim and then answered 500. A caller could cause the same 500 itself: a session subscribed to a role topic outside the alphabet (`notifications.role.bad:role`, which subscribe accepts) got it on every unsubscribe of that topic, every unsubscribe of all its topics and every `DELETE /v1/interests/<id>`, and its interest stayed. An interest or claim stored under such a key, which only a direct bucket write makes, stopped the interest reaper or the role reaper at that key every five minutes, logging `reaper cycle failed` or `role claim reaper cycle failed` at ERROR. The handle every bucket opens through now names nats.go's refusal as the refusal it is (`bus.ErrInvalidKey`, naming the key), so each of these skips the key as it already skipped one past the key bound: the claim and the unsubscribe answer 200, and the reapers go on, with a WARN naming the key they cannot delete, which an operator removes by hand (`packages/envoy/AGENTS.md`). A `/v1` route given a session id or role outside the alphabet answers 400 naming it, where it answered 500 (503 on subscribe, 404 reading the interests of a session it holds none for), and Dispatch reads a 400 or 413 from `GET /v1/interests/<id>` as no interest, as it reads a 404 (LEGION-456).
- Marking or unmarking a document's text, and checking whether a concurrent change removed the text a write inserted, walked the live tree one stack frame per level with no bound, where an authenticated peer can grow the tree through any number of small websocket updates. Each now refuses a node more than 1,000 levels deep, text included, as the document's reads do, and a peer's update that deepens the tree between a write's read and its transaction is answered `500 DOC_SCHEMA` rather than `500 INTERNAL` (LEGION-465).
- Deleting an element of a live document took one stack frame per level of nesting inside it, so an ordinary delete of a tree an authenticated peer had grown through any number of small websocket updates needed more stack than the goroutine had. Dispatch pins `github.com/reearth/ygo` to the `sjawhar/ygo` fork (at `v1.51.3-sami.1` since the entries below), which walks the deleted children iteratively and carries the transactional GC fix, both open upstream as reearth/ygo#263 and #262 (LEGION-465).
- Dispatch pins `github.com/reearth/ygo` to the `sjawhar/ygo` fork at `v1.51.3-sami.1` (commit
  `e5c1aacf`): upstream v1.51.2 and seven fixes, each open upstream as a reearth/ygo pull
  request - #258 (carried as the fork's own #262), #260, #263, #266, #268, #269 and #291
  (LEGION-535). A browser's document connection now gets a `SyncStatus` frame (Hocuspocus tag 8)
  for every SyncStep2 or Update it sends, in the order it sent them: 1 once the room applied the
  update, 0 when the room refused it (the connection stays open) or the connection is read-only
  and the update adds something the room lacks. ygo defined the tag but never sent it, so the
  dashboard's `@hocuspocus/provider` counted every edit as unsynced for the life of the
  connection. A sync frame that does not decode now closes the connection with 1002 rather than
  being dropped (#291). A room's broadcast of an update Dispatch writes is checked with ygo's
  bundled stores' check, which applies no pending cap, rather than under the server's
  `MaxPendingItems` (#268, which replaces the withdrawn #267): the room has already applied the
  update, and its document still decodes under `MaxPendingItems`. A complete state resolves its
  own dependencies through a worklist rather than a re-scan per step, so a chain-shaped state near
  the pending cap no longer takes time quadratic in its size (#260). A merged update's skip has
  parked the items after it upstream since v1.51.0 (#257). Upstream v1.51.1's depth check on the
  values `YText` stores is the entry below's.
- A mark attribute value nesting exactly 100 arrays and objects, which the schema admitted, is now outside it: ygo stores a mark as one value, the map of its attributes, so that mark reached 101 levels, and from ygo v1.51.1 (reearth/ygo#288) `YText.Insert`, `Format` and `ApplyDelta` panic on a value nested past 100. `pmdoc.Update` writes marks through all three and `MarkRange` through `Format`, so on that ygo an edit rewriting text that carried such a mark answered `500` from a recovered panic. The bound is stated once in terms of ygo's limit (`ygoValueNesting`): 100 for a node's attribute value, which ygo stores as it is, and 99 for a mark's; `MarkRange` checks the mark it is given before it writes. A stored document holding such a mark, which only a crafted client can write, is a tree outside the schema like any other: its reads and edits answer `409 DOC_SCHEMA` naming the repair, settlement writes no version of it and never writes it back, and an upload of replacement markdown repairs it (LEGION-535).
- Dispatch pinned `github.com/reearth/ygo` to the `sjawhar/ygo` fork at `v1.50.1-sami.2` (commit
  `e792b8c7`, on upstream `main` at `4d6865dc`), which adds six ygo fixes to #263 and #262, each
  open upstream (LEGION-496, LEGION-502, LEGION-484). Text no longer changes order when a document
  is encoded again: ygo folded a character into the run before it even when the two were typed
  toward different right-hand neighbours, so a browser joining a room, settlement's copy of a room
  and a compacted state could read the text in a different order from the room (reearth/ygo#266).
  An update that fills a gap in one client's updates is no longer discarded: ygo integrated a
  merged update's items past the gap, and now parks them until the gap arrives (#257). A complete
  document state no longer fails once 100,000 of its items wait on items later in the same state,
  as when a browser whose client id is lower than the server's writes more than 100,000 blocks
  into a paragraph the server wrote: ygo resolves a state's own dependencies before it applies its
  pending cap (#260). A room's broadcast of an update Dispatch writes now validates the update
  with the server's `MaxPendingItems`, the queue the room itself decodes with, rather than ygo's
  default of 100,000 (#267), so a write touching more than 100,000 of a document's existing items
  reaches the room's browsers. ygo's bundled stores keep such an update rather than refusing it
  (#268); Dispatch stores documents through its own store, which already validates under the
  room's queue, so this changes no Dispatch write. A room no browser is in, which only the API
  reads or writes, is now evicted `roomIdleTimeout` (a minute) after the API last touched it, as a
  room its last browser left already was: ygo's `Server.Apply` cleared the room's idle stamp and
  nothing set it again, so such a room stayed in memory until the process exited (#269).

- A document write whose issue closed, or whose server shut down with an editor connected, while
  the committed write was being applied to its room no longer hangs. Closing the room retired its
  persistence worker under the write, and ygo's fallback append, run on the write's own goroutine,
  waited for the write to finish first: the request that made it got no answer, every later write
  to the document waited on its writer slot, and the document's later rooms stopped persisting.
  The room's own update observer now releases the write before that append runs (LEGION-469).
- A write to a document in which a peer had deleted a chain of 200,000 nested blocks, or one whose
  lower-numbered client had written 100,000 items after one ygo could not yet place, failed
  with `fork live document: crdt: invalid update`, and so did every read of such a resident room
  once reads went through a copy. Once its room was evicted or the server restarted, the
  lower-numbered client's document could not be read or opened at all. Every decode of a
  document's state, each copy of its room and its stored history alike, used ygo's default queue
  of 100,000 items parked behind one whose parent it cannot place yet, which such a state
  overruns; each, and the room's own load, now decodes with a queue as long as one update can
  carry. The store's check of each update it appends decoded the update alone at the same
  default, so a browser update of more than 100,000 items written against blocks the document
  already held failed the room and lost the edit; that check now takes the same queue
  (LEGION-469).
- A markdown write of any shape at the 1 MiB cap no longer takes Dispatch down. One 1 MiB upload of
  `)_` held about a gigabyte while it was saved, which killed the 1,024 MiB production task
  (LEGION-481): every element the markdown makes costs memory in the parse, the read-back, the
  render and the live document. A caller's markdown - a spec, an uploaded document or version,
  an edit batch's inserts and replaces, the `find` of an edit and the quote of a comment or ask -
  may now make at most 65,536 elements, weighed as `cmd/dispatch/README.md`'s Element weights
  lists; past that a write is `413 CAP_EXCEEDED`, refused while goldmark parses, naming the line
  where the markdown passes the limit or, where the parse stopped first, the line it stopped reading
  at, and listing the weights; a quote is matched by its text alone. Every
  node the document holds other than a text is an element carrying attributes and costs about what
  two inline nodes do, so each weighs at least two: weighed as one, the heaviest document of
  `<b>a</b>` spans the limit admitted held 213 to 261 MiB to store, of `![](u)` images 327 to 383 MiB
  (and up to 1,044 MiB for four cold reads at once), and of `[^a]` footnote references 262 to
  316 MiB, past the 256 MiB a request may hold; the empty paragraph an empty list item, quote,
  footnote definition or typed block is read as holding weighed nothing, and the heaviest document
  of them held 220 to 236 MiB. An escape - a backslash before punctuation, or a character
  reference, in text outside a code span - makes no node in goldmark's tree, but the document holds
  the character it spells bare, every rendering spells it again, and the renderer reads it back as
  the delimiter or link opener it would be unescaped: a mebibyte of `\~a`, `)\_`, `\)\_` or `\[a`
  weighed four elements and allocated 330 to 720 MiB to parse and 260 to 620 MiB to render, and the
  stored `\)\_` held 256 MiB to read cold and 973 MiB for four reads at once. Every markdown file of
  4 KiB or more among this repository's 618 weighs 1.5 to 379 elements a kibibyte, so prose passes
  to the 1 MiB cap and the densest of them, a comparison matrix, to about 173 KiB; smaller files run
  denser, up to 1,421 a kibibyte for a 147-byte test fixture of empty list items. A paragraph of
  link reference definitions, which goldmark took time quadratic in its lines to read (a mebibyte of
  them took a minute to refuse), is read only as far as its first 1,024 lines.
- A write may no longer grow a stored document past what one upload may hold, measured as an
  upload is measured. Thirty-two 900 KB inserts of prose, each within both limits, grew one document
  to 29.5 MB, on which a one-word edit then held a gigabyte; a few dozen thousand headings' worth
  left one the server could not load (500 on the next write, 503 on every read); and thirty-two
  edits of an ask's options (`PATCH /api/v1/asks/{id}`) grew one to 28.8 MB, on which a one-word
  edit was killed at the production task's 1,024 MiB. Every write a
  caller makes - an upload, an edit batch, an accepted or rejected suggestion, an ask's edited text,
  its answer (`POST /api/v1/asks/{id}/answer`) or resolution, a comment's anchor mark and its margin
  record - runs in a transaction it must join (an unjoined one is refused), and is weighed by the
  markdown it leaves the document storing (what `GET .../text` answers), with an upload's own
  measures: it is `413 CAP_EXCEEDED` when that is longer than 1 MiB and longer than the document's
  was, or makes more than 65,536 elements and more than the document's did, counted as an upload's
  parse counts them (front matter apart, and a table whose short rows would take more padding than
  one write may add counted as past the limit: a 101-column header over 200 one-cell rows measured
  407 elements, the paragraph the parse leaves of it, where written whole it weighs 82,111). One
  that keeps or lowers both passes, so an over-limit document can still be trimmed or split, and any
  document a write leaves can be uploaded again from its own text. An ask's state and who answered
  it and when are the server's, not text a caller writes, so the asks of an over-limit document can
  still be answered and resolved. An answer's words and the options it chooses, whose labels are the
  asker's text, are weighed: where the document has no room for them, the ask takes the whole answer
  and its block is written without it, saying who answered and when, where one choice of a
  1,000,000-character option took a 1 MB document to 8 MB. An ask's
  edited question and options are weighed before they are made into markdown - an element for each
  backslash, `*`, `_`, `~`, backtick, `[`, `]` and `<`, which the markdown escapes or reads as
  syntax, and two for each line feed - and that markdown is parsed on the edit's element budget, so
  text past the 65,536 elements one write may make is `413 CAP_EXCEEDED` before it is rendered,
  where an option description of 400 KB of `)_` used to be taken. Each refusal's message is the
  bound's own, opening `document too large to store`, on every route. A spec, a new document or the
  markdown a rebuild is given (`POST /api/v1/artifacts/{id}/rebuild`) whose stored markdown (its
  rendering, which can run longer than what was sent) is past either limit is refused the same way,
  where a rebuild from the latest version restores it whatever it weighs. A write is also refused
  when the live state it leaves would be past the 1,048,576 structs ygo decodes in one update:
  fourteen alternating versions of 16,000 paragraphs and 16,000 headings, each within both upload
  limits, otherwise left a document every read answered 500. A document's margin - the record of
  every comment and suggestion it has had, which every load
  builds though no rendering carries it - is weighed too: one record (a comment's body and replies,
  a suggestion's replacement) holds at most 256 KiB of text and the margin 1 MiB, and a comment,
  suggestion, reply or edit that would leave either past its bound and bigger is `413
  CAP_EXCEEDED`, naming the bound and both sizes; a status change (accept, reject, resolve) is not
  text a caller writes and passes. Thirty-two open suggestions of 900 KB took one cold websocket
  load to 296 MiB, and sixty-four four cold reads at once to 1,223 MiB. The marks a document's
  comments, suggestions and asks hold on its text carry at most 1 MiB of ids and authors, which no
  rendering carries either: an anchored comment, suggestion or ask that would leave them past it and
  carrying more is `413 CAP_EXCEEDED`, where thirty asks anchored by a session whose id was 200 KB
  left a 210-byte rendering over six megabytes of live document. An answer is stored on its ask as
  well as in its block, and settlement writes it back into a block that returns to the document:
  that is weighed the same way, each returning answer on its own in document order, and an answer
  that would leave the document past either limit and bigger stays on its ask alone, its block
  restored to its state and who answered, until the document has room for it. Twenty-four answers
  of 900 KB returned by one 1,540-byte edit had left a 21.6 MB document whose cold text read held
  368 MiB.
  Browser edits over the websocket are applied before any check and are not bounded by
  this (LEGION-487). A document's stored history is bounded too: compaction runs as a room
  closes, as the server shuts down, and daily, folding every update into the document's state
  (LEGION-496), so repeated uploads of a version or replies to one comment, whose margin record
  each reply rewrites whole, cost a load only for as long as a room stays open between those -
  the same scenario that left 257 MB stored under 500 replies of 2,000 characters, with a
  one-word edit holding 1,842 MiB, before LEGION-496 now compacts to the document's own state.
- Document settlement no longer undoes an edit a browser or an agent makes while it settles
  (LEGION-479). Settlement wrote its repairs (the block ids it stamps, an ask block's server-owned
  attributes it restores) as the tree it had read before its database work, so an edit made in
  between was reverted in the live document for every connected browser, and could reach the
  stored document half applied. Each repair is now read and written in one transaction on the
  document as it stands, and so is each stamp `envoy-dispatch backfill-block-ids` writes. The
  version a repairing settlement writes is the document after its repairs, so it holds such an
  edit and credits the edit's author, whose own settlement then writes no second version. A
  repair settlement wrote into the room is committed even when the document moved after the
  write; before, it was dropped from the store while the room and its browsers kept it. A
  settlement whose room was closed and loaded again during its database work (its last browser
  left) writes nothing into the reloaded room, whose own settlement makes the repair, and logs
  `dispatch: document settlement wrote nothing into a replaced room` at INFO.
- A settlement no longer leaves a suppression slot queued that nothing releases (LEGION-479). A
  stamp that found the ids already repaired once it read the document kept its slot, and the
  room's persistence worker waited on that slot at the room's next update, so the room stopped
  storing updates. A settlement that both stamps ids and repairs an ask block held its two updates
  with one slot that matched neither, so both were stored twice and the slot stayed queued. Each
  update now has a slot of its own, and every path a repair takes releases its slot.
- A document settlement whose room closed while its repair committed (its last browser left, or
  the server shut down) no longer hangs for good (LEGION-479). Closing the document's issue never
  closes the room under a repair: the settlement holds the issue's row until it commits, and the
  close waits for that row. When the room closes under the repair, ygo stores the repair's update
  on the settlement's own goroutine once the room's persistence worker has exited, and neither
  wait there ended: the store waited for the settlement to release that update's suppression slot,
  which it does only after the store returns, and the worker's exit compaction waited for the
  room's lock, which the settlement held. Until the server restarted the document did not settle
  and none of its later updates were stored, since every update a document stores takes that lock.
  Every close of the room the server makes now waits for the repair to reach the room's
  persistence (LEGION-498, below), and the room worker's compaction skips a room whose lock is
  held, except when it evicts a failed room. The same slot wait held
  `envoy-dispatch backfill-block-ids` for good at a document whose room closed under its stamp.
- A document whose room failed while a settlement wrote a repair into it (a block id it stamps, an
  ask block's attributes it restores) no longer stays failed until the server restarts, with every
  read and write that waited for its recovery hung and none of its updates stored (LEGION-498). The
  failed room's eviction waited for the document's lock, which the settlement held, while the
  settlement waited for the room's persistence worker that the eviction had retired. A room the
  server closed under a settlement's repair while a second write committed into it, such as a
  published edit, hung the same way, each write's store waiting on the other's. The server's own
  closes of a room (a failed room's eviction, an issue's close, a shutdown's close of a room with
  an editor) now wait until a repair being written into the room has reached its persistence, and
  a repair that meets a close under way writes nothing, as one whose room was replaced does. An
  edit that reached the room's persistence just ahead of a settlement's repair is no longer dropped
  in the repair's place when the settlement discards the repair because the issue closed or the
  server began stopping.
- A document version no longer drops the credit of an edit it does not hold (LEGION-503). A
  version's commit released its authors by key, so an author already pending when the version
  took its authors who edited again before it committed lost the second edit's credit too, and the
  version holding that edit credited nobody for it: a settlement's commit did so when the edit
  landed while its update observer was between crediting and arming its settlement, and a named
  version's or snapshot's commit whenever the edit landed while the transaction held the writer
  slot. A named version or snapshot over a transaction's own write also took its authors after it
  read its tree, so an edit made in between was credited on that version, which lacked it, and on
  no other. An edit a version's tree held before its update observer had credited it (ygo runs the
  observer once the edit's transaction has released the document, and observers wait for each
  other's renders) was credited on no version: the edit's own settlement found the document
  versioned, wrote none, and released the author. Each pending author now carries the change it
  credits, every version takes its authors no later than it reads the tree it records, a version's
  commit releases only entries credited through that take, and a settlement that writes no
  version releases nothing, so the next version credits such an author. That includes an author
  whose edits came to nothing, typed and undone before a settlement. An upload that changes the
  document clears every credit pending at its write's room read, whether its replacement removed
  that edit or kept it, and its version credits its uploader alone; an edit credited after that
  read stays pending for the next version, and an upload that changes nothing clears nothing. A new
  ask is attributed to whoever introduced its block: a service edit's, an upload's or a committed
  transaction's own before/after trees name the ids it adds, staged on the write and registered
  into the room's bookkeeping only once the write commits, never before - discarding the
  transaction, or refusing the write for any other reason, leaves no trace, so a later, separately
  committed write of the same author-chosen literal id is never outranked by one that never reached
  the room. Registration happens before the update can reach any observer, rather than
  whichever update's observer happens to render a merged catch-up first; an id no write registered
  this way is a browser's, named for the one browser connected when its update arrived, or the
  document-settlement actor when several were. A settlement's or the block-id backfill's own id
  repair, and an edit's repair of an existing, unrelated block, each carry forward the author
  recorded for the id a rename replaces, unless it is a copy of the block that keeps that id - on a
  write joined to a transaction too, where the write's own generic before/after diff would
  otherwise count the renamed id as newly added and claim it for the write's own actor instead. A
  copy's previous id stays live in the room's own bookkeeping, since another block still carries
  it; any other rename's does not, closing a window where a later block reusing that literal id
  could be mistaken for the one just retired. A rename whose own update's observer had not yet
  recorded an author is named after the settlement's own actor instead. Before, a settlement named
  its own actor - the latest editor, or the first pending author - so a browser edit elsewhere
  could take an agent's ask, and a block that arrived while the settlement ran could take the name
  of an earlier editor; later, two updates landing while one's own observer renders a catch-up that
  includes both still let whichever ran first claim both blocks; later still, a discarded write's
  registration could outrank a later write's legitimate one, and a conditional edit's own repair
  of a copied or unrecorded rename could still be claimed by the write's own actor instead of
  falling through correctly. A block whose update the observer has not rendered yet waits for the
  settlement that observer arms; a block the room held when it loaded is still named as the
  settlement's other events are. Approval moves name the actor whose edit moved the version, even
  when it credits several authors, so a stale pending author does not make a human's move appear as
  the document settlement or suppress its notification.
- A GitHub App response over 1 MiB now fails whole instead of returning a truncated body. The
  dashboard proxy answers `502 GITHUB_UPSTREAM` and names the 1 MiB limit.
- Saving a document, comment, ask, or message with a long run of underscore-joined characters
  no longer takes quadratic time in Postgres search indexing. `pmdoc` also avoids quadratic work
  in Goldmark's email and delimiter scans and in renderer closer scans. A document that exceeds
  one update's 1,048,576-item cap is now refused as `413 CAP_EXCEEDED` with its item count instead
  of `500` (LEGION-465).
- A Dispatch shutdown no longer drops a document settlement its budget cuts short. The settlement
  was armed only in memory, so a deploy that stopped the server while a large document settled
  (a 1 MiB `a_b*` document took 4.5-6.8 s at load 90-170, past the 5 s budget) left its ask blocks
  unindexed and an edit's version unwritten until the next edit. Every durable document update
  now records the settlement it owes in `doc_settlements_pending` (migration 0063), which the
  settlement deletes when it commits. A room's load settles a document with that row, and the
  server arms the settlement of every open document whose row is a minute old, at start and each
  minute, so one nobody opens settles too. Shutdown settles only the documents that owe one,
  cancels a settlement past its budget so its transaction rolls back, and logs per document
  whether it settled or was left to resume (LEGION-465).
- Dispatch indexes a mention written in markdown (LEGION-463): `**dispatch://KEY**`,
  `` `dispatch://KEY` ``, `_dispatch://KEY_`, `~~dispatch://KEY~~`,
  `[dispatch://KEY](dispatch://KEY)` (how a document stores `<dispatch://KEY>`), a bracketed
  dashboard URL, a reference in a table cell written without padding (`|dispatch://KEY|`), and a
  reference followed by a no-break or ideographic space each write their `mentions` edge. A
  reference ends at whitespace, an angle or square bracket, a quote, a backtick or a pipe, except
  the bracketed host of an IPv6 dashboard URL, and the sentence punctuation, emphasis delimiters
  and unbalanced `)` after it are dropped in one pass, where a spec of one reference and 900,000
  `)` took 21 s to save. The server parses a reference as the dashboard does: a query's pairs
  split on `&` alone, an item id decoded, a slug only whole, a version without a leading zero, and
  nothing from a reference holding a control character or an item id that decodes to one. A body
  citing `…/spec?comment=%00` was answered 500, since Postgres refuses a NUL in the index, and is
  now stored. The dashboard's composer pills, unfurl cards and linked text read text by the same
  rule. Text stored before the deploy gets its edges from `envoy-dispatch rebuild-refs`.
- The dashboard no longer hangs on a comment or an ask question made of repeated `http://`. The
  check that shows an unfurl card only for a body of nothing but references tried every way to
  split the body into references, which in Chromium ran for minutes on a 2,000-character comment.
- During a rolling deploy the replacement listener serves `/v1` and runs its role lane once NATS
  is connected and the interest and session caches are warm (about 100 ms after it listens),
  instead of answering every `/v1` call `503 service starting` until the old task let go of the
  durable consumer (84 s in production on 2026-10-01). The durable's bind is polled every 2 s and
  lands within 2 s of the old task's exit, still giving up after 135 s with
  `subscribe failed after max attempts, shutting down`; `/healthz` stays 200 `starting` until it
  binds. Both tasks of a machine are one role-lane queue group, so the overlap forwards no role
  message twice. A SIGTERM during the bind wait is an ordered shutdown. The bus now restores every
  subscription after a reconnect even when one cannot be restored, so a role lane is not left down
  while the durable still refuses the bind (LEGION-456).
- Dispatch exits with status 1 when it cannot bind its listen address. It logged
  `dispatch: listen … bind: address already in use` and exited 0, so a supervisor read a port
  clash as a clean stop.

- A Dispatch request refused because its document room failed answers
  `503 DOC_SERVICE_UNAVAILABLE` whatever failed the room, as a comment's or ask's
  `anchor_block_error` names it: both name a document error through one classification
  (`api.documentErrorCode`), which takes a failed room before any cause the room carries. A room
  failed by settlement's schema refusal answered `500 DOC_SCHEMA`, though the request had not met
  that refusal itself; a retry once the room is evicted meets the document (LEGION-460).

- A search that contains only stop words now returns `200` with no results, so every consumer
  can show an empty result rather than a retryable failure.

- A targeted message's delivery claim no longer deadlocks with the session's reply to the same
  message: it takes the message row `FOR NO KEY UPDATE`, as the new accept does, which the
  reply's foreign-key `FOR KEY SHARE` does not wait for (LEGION-394).
- Dispatch no longer stores a table row without its last cells. A cell ends at every `|` not
  written `\|`, inside code and links too, and the parser dropped the cells a body row held past
  its delimiter row's, which the browser editor's parser keeps: a spec, upload or version whose
  code span held a bare `|` in a row that filled its table was stored with the rest of that row
  gone, and no refusal. Such markdown is now refused (`400 INVALID_MARKDOWN`; an insert's
  `markdown` answers `INVALID_OP`), naming the row's cells, its table's width and its opening
  words, and both fixes: write a `|` inside a cell as `\|`, or give the header and delimiter rows
  as many cells as the row. A row whose cells past the width are all blank is still read at the
  table's width. A bare-row insert now decides in three steps. A fragment whose every line yields
  a cell and an unescaped `|` (a lone `|` yields no cell, and a line whose only pipe is `\|` has no
  separator), with no line delimiter-shaped (cells of three hyphens or more), is parsed under a
  header at the target table's width. What that parse refuses is refused, a wide row of that
  table as `TABLE_WIDTH`. When it refuses nothing and reads one table holding every line, those
  rows are inserted. Every other fragment goes to the block path whole, which reads it as a
  document of its own. The insert used to count cells itself first, line by line, over a fragment
  trimmed of Unicode spaces and a first row's indentation, so it now answers differently in these
  ways. It drops blank cells past the width and refuses text there, where it refused both. A `|`
  after an even run of backslashes is text to it, as it always was to uploads, so
  `| A11 | new \\| extra |` under two columns is stored as one cell reading `new \| extra` where it
  was refused as `TABLE_WIDTH`; the browser editor's reading of such a pipe is LEGION-412. A space
  outside ASCII, a vertical tab or a form feed is a cell's text wherever it stands, so
  `| A11 | new |` then U+00A0 is refused as three cells where it was stored as two, and a line of
  `|` then U+00A0 is a row holding it, as an upload reads it, where it was a lone `|` that sent the
  fragment to blocks. A line the parser refuses as another block (indented code, `2. | a | b |`)
  is refused as an upload refuses it, `INVALID_OP` on `markdown`, however many cells it holds and
  on any row. A fragment that goes to the block path was refused as `TABLE_WIDTH` wherever
  the old count met a line too wide before whatever sends the fragment there, as with
  `- | a | b |` (counted as three cells), or `| A11 | x | y |` then a lone `|` or a line whose only
  pipe is `\|` under two columns; it is now written as the blocks it reads as.
  The route also tries a fragment as rows before it reads it as a document of its own, so rows
  that reading refused, such as `[x]: |`, which it took for a link reference definition, are
  inserted as the rows an upload reads. Versions written before 2026-09-19 hold rows with text
  past their table's width, since the renderer then wrote a code span's pipe unescaped;
  re-uploading one is refused rather than stored short. A row's closing `|` after an odd run of
  backslashes (`| x | y \|`) is now kept as the last cell's text, as the browser editor reads it,
  where it was dropped (`y \`). An image's alt holding a backslash before a pipe in a table cell
  is written so that the browser editor reads it as one cell too.

- `GET /api/v1/broadcasts/{id}` now returns recipient copies in the order the sender named them,
  including the relative order of recipients left after exclusions. Broadcasts created before
  this ordering was stored retain their existing timestamp-and-UUID fallback order.

- A listener restart no longer replays the `envoy_interests` bucket's delete markers before it is ready, because each listener now collects them. Every delete path left one (the reaper for each dead session, an unsubscribe-all, the admin delete) and nothing ever removed one: production's bucket held 41,873 subjects for 40 live keys, and the on-prem listeners spent 17-25 s of every restart with no deliveries. A pass reads the stream, scans the bucket MetaOnly, takes the lowest revision that scan delivered as a PUT, reads the stream again and purges it below that floor — a floor from the stream, not from the cache, so a live key this build cannot decode is protected and a partial cache cannot raise it. It purges only when the scan completed, it saw a live key, the second read is the stream the first read read (the same creation time), the sequence space only moved forward between them, and `FirstSeq < floor <= LastSeq`; otherwise it refuses, at WARN when a read failed, the scan did not complete, the stream was replaced, the sequence space moved backward, or the floor is above the stream's last sequence, and at DEBUG when there is nothing below the floor or no live key at all. Every pass logs every stream value the decision used (`read1_*`, `read2_*`, `floor`, `live`, `delete_markers`), plus `purged`, `purged_first_seq` and `purged_msgs` when it purged; `purged` is approximate both ways, since a concurrent put under-reports it and a peer listener's purge in the same moment is counted by both passes. Every listener runs it after its interest cache's first warm-up and then every five minutes; the purge is idempotent, so no leader is needed. Measured with the listener binary on a bucket seeded with 500 markers behind 3 live keys: `purged=500`, the bucket down from 503 subjects to 3, every live key at its stored revision, and the next restart's warm-up line reporting `delete_markers=0`. It adds one NATS capability, publish on `$JS.API.STREAM.PURGE.KV_envoy_interests`; a user without it leaves every marker in place, logs `interest marker collection could not purge the bucket` once and keeps serving.
- A listener that mounts no GitHub webhook route (`ENVOY_WEBHOOKS` without `github`, as on the on-prem fleet) no longer opens the CI store: it creates no `envoy_ci_state` watch or bucket and runs no CI summary loop, and `/healthz` reports `"ci_cache": "not_applicable"`. The watch's initial scan of every record (62 MB in production) over a Tailscale DERP relay held up the listener's other startup requests past their 10 s deadline, so an on-prem listener that restarted either exited (`context deadline exceeded`) or stayed unhealthy and delivered nothing. CI settlements are published by the listeners that receive GitHub webhooks; one that mounts the route behaves as before.
- A listener restart no longer spends one NATS round trip per stored role claim before it is ready. `store.Open` snapshots every claim's revision — the grace a restored holder gets to register again — from one watch over the `envoy_roles` bucket's existing keys, as the interest, session and CI caches read theirs, instead of listing the keys and then reading each one. Measured with the listener binary against a link relayed at about 40 ms round trip and a bucket holding 766 claims: 35.6 s from `webhooks open` to a healthy `/healthz` before, 1.2 s after, against 0.9 s with an empty bucket. Restored claims keep today's behaviour, including the grace window and the claim stored under a key this build cannot read. It also closes a defect the per-key read had: a scan that does not reach the end of the bucket now fails the start naming how many keys it read, rather than opening on a partial snapshot whose missing holders lose their grace a session TTL early. nats.go ends a scan early two ways and only one looks like a failure — it closes the updates channel when the watcher's subscription ends, and on its own idle timeout (the JetStream `MaxWait`, 10 s here) it sends the same marker a complete scan ends with, reporting the timeout only on `Error()`, which the scan now reads at the marker. `nats.KeyValue.Keys()` carries that timer too, so a listener on the released build restores a partial snapshot over a link that goes quiet for the `MaxWait` and then recovers: measured against a link held 13 s mid-scan, 232 of 400 claims restored with no error. The fix reaches production with the deployment's listener pin bump that carries this build.
- A role claim that overtook another session no longer risks deleting a newer claim made in the same instant; old-holder cleanup now touches only the previous holder's topics, never the role row.
- Document version mutations now emit one retained, non-notifying `comment.anchor_refreshed` or `ask.anchor_refreshed` event for each changed open anchor, so stream consumers immediately observe orphaned quotes.
- A webhook redelivery of an event the notification stream already holds no longer publishes a second copy. Webhook envelopes whose dedupe key is the source plus the upstream delivery id (GitHub's `X-GitHub-Delivery`, Slack's `event_id`, Ghost Wispr's `X-GhostWispr-Delivery`) now publish under a JetStream MsgId of that key and topic, the rule Dispatch envelopes already used. Other producers that publish under those source names - CI settlements, the MCP bridge and `/v1/messages/*` callers - still publish every time. GitHub documents that a redelivery carries the original `X-GitHub-Delivery`, and Slack retries under the original `event_id`; that Ghost Wispr resends under its original delivery id is assumed, not documented.
- A GitHub delivery no longer makes an envelope larger than NATS takes, which a body up to the 25 MiB cap could: every text a webhook envelope copies is capped at 2,048 runes, a push's changed paths at 32,768 runes as well as 100 paths, and every text a check run copies into the CI store at 2,048 runes, a name past that keeping a digest of the whole name so two names sharing their first 2,048 runes stay two checks; a ref or workflow file name writes whitespace, `*` and `>` as `_` in its topic. What NATS still refuses however often it is sent (a subject past the server's 4 KiB protocol line, over which the server closed the listener's NATS connection, a message past the server's max payload, or a subject holding whitespace or an empty token) is answered 422 and logged `<source> publish refused` rather than 503 and `<source> publish failed`, so Dispatch's sweep does not redeliver it and the publish-failure line does not fire. A head's CI record is bounded at 384 KiB and its settlement at 960 KiB; a check past either is refused the same way and that head never settles from the listener, and a settlement NATS refuses anyway marks its head the same way instead of being published again every second. `/v1/messages/publish` and `/v1/messages/send` answer a message NATS cannot take whole with 413 naming its size. Every KV handle checks its keys before sending (a key long enough to close the connection, or holding an empty token, is refused), so a `/v1` session id or role NATS would refuse is a 413 or 400 rather than a 503 after the listener's NATS connection closed. A write is held to the longest subject its key makes; a key an earlier build stored past that bound still lists and deletes, the stores skip it where they cannot read it, and the reapers delete it, so such a key never stops a listener from starting. Dispatch's outbox logs a destination NATS refuses once and counts it done instead of retrying it forever.
