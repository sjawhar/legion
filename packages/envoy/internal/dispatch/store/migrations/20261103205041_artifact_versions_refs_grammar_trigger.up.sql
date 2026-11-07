-- 20261103205041_artifact_versions_refs_grammar_trigger.up.sql
-- Every document version any build writes owes its document an index with the whole grammar and
-- owes that document's scope one (20261103205037's refs_grammar_owed and
-- refs_grammar_owe_version): the version's own text is read again with the bare keys, `#id`s and
-- short ids it writes, and its question blocks are the map every `#id` written in its scope
-- resolves against, which this version can have added, renamed or dropped. An image or file
-- version carries no markdown and owes nothing, which is 0122's predicate.
-- A version this build writes indexes its own text in the same transaction, which deletes the
-- document's row (refs.replaceRefs); the scope row it leaves is the work the sources around it
-- are owed, and the drain writes only the ones whose text holds a `#id`.
-- Its own migration, so the transaction holds artifact_versions' lock alone (0122's precedent).
create trigger artifact_versions_refs_grammar_owed after insert on artifact_versions
  for each row when (new.markdown is not null) execute function refs_grammar_owe_version();
