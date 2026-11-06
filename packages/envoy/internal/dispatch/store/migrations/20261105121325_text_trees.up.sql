-- 20261105121325_text_trees.up.sql — the display tree of every stored text, so a read serves
-- stored bytes instead of reading the markdown again.
--
-- sjawhar/legion#2712 (INFRA-635): a response carries the display tree of every stored text it
-- answers with (api's display_trees.go), and until now every one of those trees was read from the
-- markdown on the request, kept only in the 64 MiB byte-LRU one process holds
-- (pmdoc.DisplayCacheBytes), which a restart loses. A tree stored here outlives the process: the
-- write that stores a text stores its tree beside it, and a read copies these bytes.
--
--   sum         the SHA-256 of the text, exactly as pmdoc keys its own cache of the trees it reads
--               (displayCacheKey). Keying on the text rather than on the row that holds it means a
--               quoted reply, a question an edit carried over and two identical option labels
--               share one row, and that a text written twice is read once.
--   mode        the display mode the tree was read in: pmdoc.DisplayLatest (0) or
--               pmdoc.DisplayVersion (1). The two readings of one text are two trees
--               (pmdoc.ReadForDisplay), so the mode is part of the key.
--   version     pmdoc.DisplayTreeVersionNow(), the display parser version of the build that read
--               it. A stored tree is served only while the version that stored it stands at the
--               number the reading build holds, so a change to what a reader is shown raises that
--               number and every tree stored under the old one is simply never read again.
--   tree        the encoded tree, the bytes pmdoc.DisplayJSON answers, which is what a read puts in
--               the response: a hit copies them and parses nothing.
--
-- One row holds one whole text's tree, never one block's: a reader is given the tree of the text
-- it reads, and a tree cut per block would have to be joined on every read.
--
-- text_tree_rebuild is the background pass's own record, one row per display parser version: how
-- far through the stored texts that version's pass has reached (through) and whether it has
-- finished (done). It holds no row until a pass writes one.
--
-- One index beside the primary key, on version alone. A read names the whole key - `where
-- version = $1 and mode = $2 and sum = any($3)` - and reads the primary key by its leading
-- column. The background pass's hourly retention names version and nothing else, on both of its
-- statements: the two newest versions the table holds (`select distinct version ... order by
-- version desc limit 2`) and the delete of everything below them (`where version < ...`). The
-- primary key leads on sum, so neither statement can use it, and each would read the whole of a
-- multi-gigabyte table every hour to delete the nothing a deploy that shipped no parser change
-- leaves behind (texttreefill's retainQuery).
--
-- Two new tables, nothing altered: the release before this one neither reads nor writes them, so
-- it rolls beside this one and reads and writes this schema as it always did (store/README.md).
-- The census answers 0, no row of any existing table being read, refused or rewritten, and rolling
-- back past it is an image rollback alone.
create table text_trees (
    sum bytea not null,
    mode smallint not null,
    version integer not null,
    tree bytea not null,
    created_at timestamptz not null default now(),
    primary key (sum, mode, version)
);

create index text_trees_version on text_trees (version);

create table text_tree_rebuild (
    version integer primary key,
    through timestamptz not null,
    done boolean not null default false
);
