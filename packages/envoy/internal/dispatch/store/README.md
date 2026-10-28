# Dispatch store

`migrations/` holds every `<version>_<name>.up.sql` boot applies in version order, with the
`<version>_<name>.census.sql` each one declares for the pre-deploy census. This file sits beside
that directory rather than in it because the directory is embedded with `all:` and
`pgmigrate.Load` refuses a set holding a file named anything but `<version>_<name>.up.sql`,
`.down.sql` or `.census.sql`, which would refuse every boot.

**A migration must leave the release before it able to read and write the schema.** A Dispatch
release rolls, so the old build and the new build run against this schema at once, and nothing at
boot refuses a schema newer than the binary knows. A change is therefore expand-then-contract
across two releases: the expanding migration adds, with a default or a backfill, and the release
that ships it writes both shapes, while the migration that drops, renames or narrows ships no
earlier than the next release, once no running build reads the old shape. No migration is an
exception: a change that cannot be written that way is split into releases that can, so no
release stops every server process. The same rule holds for everything else the two builds share
(request bodies and routes, query values, socket frames, response members, stored documents, NATS
stream and consumer settings and subjects, messages, and settings), and the two-build test checks
it (`go test -tags twobuild ./cmd/dispatch`). **Messages:** envelope and payload members are
additive, and every consumer ignores a member it does not know; a consumer ignores an event type
or `kind` it does not know, and acks it; a member or a kind is removed only after no running build
reads it. The decoders that make this true are the listener's (`cmd/listener/delivery.go`), the
session plugins' (`packages/envoy-client/src/delivery.ts`, `renderInbound`), the Legion daemon's
(`packages/daemon/internal/intake/decode.go`) and the delivery intakes'
(`internal/dispatch/delivery/intake.go` and `jobs.go`).

`../../../cmd/dispatch/README.md` has the rule for each surface ("Two builds at once") and for
migrations with their examples ("Database migrations"), the naming and numbering a set is refused
for, and what the census is held to.
