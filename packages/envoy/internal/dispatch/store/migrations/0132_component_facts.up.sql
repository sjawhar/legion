-- 0132_component_facts.up.sql
--
-- When a component was made and last changed (its graph node's made_at and changed_at). Every
-- import deletes a source's components and inserts them again (architecture/importer.go), so no
-- components row keeps a time; component_facts does, keyed as components are. digest is the SHA-256
-- of the component's own fields at its last recorded change, which tells the importer whether an
-- import changed it.
--
-- No foreign key reaches components: its rows are deleted and inserted again on every import,
-- which a cascade would carry into these rows. A component that no import holds any more loses its
-- row to the importer instead. The key to projects cascades as components' own does, so the rows go
-- with their project.
--
-- Every component already stored gets a row with all three null: when it was made or last changed
-- was never recorded. Reads components (ACCESS SHARE) and writes only the table it creates; the key
-- to projects takes SHARE ROW EXCLUSIVE on projects for the catalog change.
create table component_facts (
  project_key text not null references projects (key) on delete cascade,
  id text not null,
  made_at timestamptz,
  changed_at timestamptz,
  digest text,
  primary key (project_key, id)
);

insert into component_facts (project_key, id) select project_key, id from components;
