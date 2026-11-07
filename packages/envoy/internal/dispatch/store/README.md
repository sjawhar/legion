# Dispatch store

`migrations/` holds every `<version>_<name>.up.sql` boot applies in version order, with the `<version>_<name>.census.sql` each one declares for the pre-deploy census.
`<version>` is one of the numbers the set grew up with, each of whose files keeps the number it landed with, and for every migration added since the UTC moment its author created it, `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`):
two pull requests open at once cannot both claim one timestamp, while both would claim the next number.
This file sits beside that directory rather than in it because the directory is embedded with `all:` and `pgmigrate.Load` refuses a set holding a file named anything but `<version>_<name>.up.sql`, `.down.sql` or `.census.sql`, which would refuse every boot.

Boot applies every version `schema_migrations` lacks, in version order, so a fresh database applies the whole set in that order, and a migration that merges below one a database has already applied still runs there, once.
A deployed database applies a migration when the release carrying it deploys:
if T2's release deploys before T1 merges, that database applies T2 then T1 while a fresh one applies T1 then T2.
So a migration must not depend on its order relative to one written in a concurrent pull request, another branch open at the same time;
one that needs another's effect is written after that one has merged.
`max(version)`, the `schema_version` `/healthz` reports, is the highest version applied, not proof that every version below it is applied, which is why the auto-deployer's rollback guard compares the whole set of recorded versions.

**A migration must leave the release before it able to read and write the schema.**
A Dispatch release rolls, so the old build and the new build run against this schema at once, and nothing at boot refuses a schema newer than the binary knows.
A change is therefore expand-then-contract across two releases:
the expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes, while the migration that drops, renames or narrows ships no earlier than the next release, once no running build reads the old shape.
No migration is an exception:
a change that cannot be written that way is split into releases that can, so no release stops every server process.
The same rule holds for everything else the two builds share (request bodies and routes, query values, socket frames, response members, stored documents, NATS stream and consumer settings and subjects, messages, and settings), and the two-build test checks it (`go test -tags twobuild ./cmd/dispatch`).
**Messages:** envelope and payload members are additive, and every consumer ignores a member it does not know;
a consumer ignores an event type or `kind` it does not know, and acks it;
a member or a kind is removed only after no running build reads it.
The decoders that make this true are the listener's (`cmd/listener/delivery.go`), the session plugins' (`packages/envoy-client/src/delivery.ts`, `renderInbound`), the Legion daemon's (`packages/daemon/internal/intake/decode.go`) and the delivery intakes' (`internal/dispatch/delivery/intake.go` and `jobs.go`).

`../../../cmd/dispatch/README.md` has the rule for each surface ("Two builds at once") and for migrations with their examples ("Database migrations"), the naming and the identifiers a set is refused for, and what the census is held to.
