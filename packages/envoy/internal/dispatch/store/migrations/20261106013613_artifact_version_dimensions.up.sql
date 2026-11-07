-- 20261106013613_artifact_version_dimensions.up.sql — a picture's own width and height.
--
-- INFRA-760 R12: a document stops moving as its pictures load. A browser reserves a picture's box
-- before its bytes arrive only when it is told the picture's shape, so the shape is read once,
-- where the upload already holds the whole file in memory and sniffs its type
-- (internal/dispatch/api/artifacts.go, artifactKind), and kept beside the type and the byte count
-- it was read with.
--
-- Both columns are nullable, and nothing backfills them here. A version stored before this
-- migration has no recorded shape, and so does one whose header does not decode and one that is
-- not a picture at all - a video, a document, any other file - so "no shape recorded" is a state
-- every reader must already answer for, and a default or a NOT NULL would make it unsayable.
-- Nullable also makes this an add-column with no default: it writes the catalog alone, holding
-- artifact_versions' ACCESS EXCLUSIVE lock for milliseconds and rewriting no row.
--
-- A migration must leave the release before it able to read and write the schema
-- (internal/dispatch/store/README.md). Every insert that release makes names its columns and
-- names neither of these, and a column with no default admits that insert, so the old build goes
-- on storing versions with no shape while the new one records it.
--
-- integer, not smallint: maxArtifactBlobSize is 25 MiB, which a picture far wider than 32767
-- pixels fits inside, and a header that claims a width Postgres cannot store would refuse the
-- upload. Its identifier is the UTC moment it was written, as `migrationid` requires of every
-- migration past the numbered series, which closed at 0161 (cmd/dispatch/README.md, "Database
-- migrations").
alter table artifact_versions
  add column width integer,
  add column height integer;
