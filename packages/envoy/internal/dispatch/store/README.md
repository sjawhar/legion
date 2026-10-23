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
earlier than the next release, once no running build reads the old shape. A migration that cannot
be written that way is a stop-the-world migration, deployed with no server of the earlier release
running, and that is an exception named in the pull request, not a default.

`../../../cmd/dispatch/README.md` ("Database migrations") has the rule with its examples, the
naming and numbering a set is refused for, and what the census is held to.
