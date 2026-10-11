-- 0090_search_vector_warning.up.sql
-- A text search_vector cuts to fit says so (LEGION-505).
--
-- search_vector (0087) builds a row's vector from the longest of its body's first half, quarter,
-- eighth, ... that fits Postgres's limit on one tsvector, and until now did so silently: the write
-- succeeded and nothing recorded that keyword search would not find the text's later words. It now
-- raises one WARNING per call that cuts, after the cut it keeps, naming the body's length, how much
-- of it the vector holds and the limit, never the text. Dispatch logs every Postgres warning a
-- connection receives at WARN with the context Postgres attaches (store.logNotice), which names the
-- trigger function, and the server log keeps it too. An issue title is cut twice on one write, once
-- for its vector (issues_search) and once for its lexemes (title_lexemes, 0088), so it raises two;
-- titles are capped at 1,000 characters (contracts.IssueTitleMax), far short of the limit, so only
-- a title written straight into the table can.
--
-- No RAISE here, or in any migration or function one defines, embeds caller-written text: a notice
-- reaches Dispatch's log whole (cmd/dispatch/AGENTS.md).
--
-- The body is 0087's but for that: the loop leaves with the vector instead of returning from inside
-- its exception block, so the warning is raised once, outside it. A text that fits takes the first
-- try and raises nothing. create or replace keeps every trigger bound to the function, so this
-- takes no lock on any table and changes no stored row; 0087 itself is left as it ran.
create or replace function search_vector(head text, body text) returns tsvector
language plpgsql immutable strict as $$
declare
  -- Postgres's limit on one tsvector's lexemes and positions (MAXSTRPOS, src/include/tsearch/ts_type.h),
  -- past which to_tsvector raises program_limit_exceeded.
  tsvector_byte_limit constant integer := 1048575;
  head_vector tsvector := to_tsvector('english', head);
  kept integer := length(body);
  part text := body;
  vector tsvector;
begin
  loop
    begin
      vector := head_vector || to_tsvector('english', part);
      exit;
    exception when program_limit_exceeded then
      if kept = 0 then
        raise;
      end if;
      kept := kept / 2;
      -- One character past the cut, so a word that runs past it is dropped whole.
      part := regexp_replace(left(body, kept + 1), '\S*$', '');
    end;
  end loop;
  if kept < length(body) then
    raise warning 'search_vector cut a text of % characters to its first % to fit the %-byte limit on one tsvector',
      length(body), length(part), tsvector_byte_limit;
  end if;
  return vector;
end $$;
