-- 0062_search_reindex.up.sql
-- LEGION-465 part 7 of 7: re-index the rows whose vector search_text could change.
--
-- Only a text holding a run of sixteen or more underscore-joined segments reads differently through
-- search_text (0056). A no-op update of the text fires the table's trigger (0057-0061) for those
-- rows and nothing else; an UPDATE takes ROW EXCLUSIVE on its table, which blocks no read and no
-- write of another row, and row locks on the rows it rewrites until this transaction commits. Of
-- 3,336 real documents none changed its vector; on a copy of the five tables at five times
-- production (845 MB) the five scans took 3.4-3.8 s under ROW EXCLUSIVE alone, re-indexing 122
-- rows, while concurrent reads and writes of every table answered at once. Every other row keeps
-- the vector it has, which the new expression reproduces exactly.
update issues set title = title where title ~ '(?:[A-Za-z0-9.-]+_){16}';
update artifact_versions set markdown = markdown where markdown ~ '(?:[A-Za-z0-9.-]+_){16}';
update comments set body = body where body ~ '(?:[A-Za-z0-9.-]+_){16}';
update asks set question = question
 where (question || ' ' || ask_search_suffix(options, answer)) ~ '(?:[A-Za-z0-9.-]+_){16}';
update messages set body = body where body ~ '(?:[A-Za-z0-9.-]+_){16}';
