-- 0090_search_vector_warning.up.sql
-- A text search_vector cuts to fit says so (LEGION-505).
--
-- search_vector (0087) builds a row's vector from the longest of its body's first half, quarter,
-- eighth, ... that fits Postgres's limit on one tsvector, and until now did so silently: the write
-- succeeded and nothing recorded that keyword search would not find the text's later words. It now
-- raises one WARNING per cut, after the cut it keeps, naming the body's length, how much of it the
-- vector holds and the limit, never the text. Dispatch logs every Postgres warning a connection
-- receives at WARN (store.logNotice), and the server log keeps it too.
--
-- The body is 0087's but for that: the loop leaves with the vector instead of returning from inside
-- its exception block, so the warning is raised once, outside it. A text that fits takes the first
-- try and raises nothing. create or replace keeps every trigger bound to the function, so this
-- takes no lock on any table and changes no stored row; 0087 itself is left as it ran.
create or replace function search_vector(head text, body text) returns tsvector
language plpgsql immutable strict as $$
declare
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
    raise warning 'search_vector cut a text of % characters to its first % to fit the 1048575-byte limit on one tsvector',
      length(body), length(part);
  end if;
  return vector;
end $$;
