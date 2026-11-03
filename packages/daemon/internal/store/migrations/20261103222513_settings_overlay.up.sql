-- 20261103222513_settings_overlay.up.sql — the settings changes the daemon took from its route.
--
-- The daemon resolves `legion.yaml` under this overlay (config.Live): a change an operator or
-- Dispatch made through `PUT /legion/v1/operator/settings` is laid over the file's own value and
-- wins, so the change survives the daemon's next restart, which a person making it from the
-- Legion view expects of it. One row per dotted path, per project: one database serves every
-- project's daemon, and each daemon reads and writes its own project's rows alone.
--
-- value is the value the route asked for, as JSON, which the daemon lays over the file's document
-- before the loader reads the two layers as one file. source is `route` for every row today —
-- the file's own values are no overlay and are never stored — and is kept so a reader of a row
-- sees what a Change carries. changed_by is the person the caller named, and changed_at when the
-- daemon applied it; the state reports both beside the setting (api.SettingProvenance.Restore).
--
-- file_digest is the sha256 of the file's own value for that path at the moment the change was
-- applied, as config.Live.FileValues encodes it ("" for a path the file did not carry). It is
-- what tells a route change still standing from one the file has taken back: the latest change of
-- a key wins, so when the file's own value for a path has moved since, the file changed that key
-- and its row is deleted rather than laid over it. A digest, not the value, because the file's
-- value may be a credential or an address carrying one, and equality is all this comparison needs.
create table settings_overlay (
  project text not null,
  path text not null,
  value jsonb not null,
  file_digest text not null,
  source text not null,
  changed_by text not null,
  changed_at timestamptz not null,
  primary key (project, path));
