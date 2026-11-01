create function ask_search_suffix(options jsonb, answer jsonb) returns text
language sql immutable parallel safe as $$
  select coalesce((
    select string_agg(concat_ws(' ', option->>'label', option->>'description'), ' ')
    from jsonb_array_elements(
      case when jsonb_typeof(options) = 'array' then options else '[]'::jsonb end
    ) as option
  ), '') || ' ' || coalesce(answer->>'text', '') || ' ' || coalesce((
    select string_agg(selected, ' ')
    from jsonb_array_elements_text(
      case when jsonb_typeof(answer->'selected') = 'array' then answer->'selected' else '[]'::jsonb end
    ) as selected
  ), '')
$$;

drop index asks_search;
alter table asks drop column search;
alter table asks add column search tsvector generated always as
  (to_tsvector('english', question || ' ' || ask_search_suffix(options, answer))) stored;
create index asks_search on asks using gin (search);

alter table artifact_versions drop column search;
alter table artifact_versions add column search tsvector generated always as
  (to_tsvector('english', regexp_replace(markdown, '(?s):::ask\{.*?:::', '', 'g'))) stored;
