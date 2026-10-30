-- 0150 creates four tables, ask_blocks, ask_block_texts, ask_block_state and ask_block_refs, and
-- their indexes (ask_blocks_node_id, ask_blocks_ask_id_text, ask_block_state_owed,
-- ask_block_refs_to, ask_block_refs_from_node), and re-states graph_edges, which changes no
-- stored row. It adds no column and alters no existing table, and it inserts no row: it refuses
-- and rewrites no existing row.
select 0
