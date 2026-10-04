-- Rows whose search vector 0062's re-index changes: those holding sixteen or more
-- underscore-joined segments whose stored vector differs from the new expression's. A non-zero
-- count names documents to inspect before the deploy.
select count(*) from (
  select 1 from artifact_versions where markdown ~ '(?:[A-Za-z0-9.-]+_){16}'
     and search is distinct from to_tsvector('english', regexp_replace(regexp_replace(markdown, '((?:[A-Za-z0-9.-]+_){16})', '\1 ', 'g'), '(?s):::ask\{.*?:::', '', 'g'))
  union all
  select 1 from comments where body ~ '(?:[A-Za-z0-9.-]+_){16}'
     and search is distinct from to_tsvector('english', regexp_replace(body, '((?:[A-Za-z0-9.-]+_){16})', '\1 ', 'g'))
  union all
  select 1 from messages where body ~ '(?:[A-Za-z0-9.-]+_){16}'
     and search is distinct from to_tsvector('english', regexp_replace(body, '((?:[A-Za-z0-9.-]+_){16})', '\1 ', 'g'))
  union all
  select 1 from asks where (question || ' ' || ask_search_suffix(options, answer)) ~ '(?:[A-Za-z0-9.-]+_){16}'
     and search is distinct from to_tsvector('english', regexp_replace(question || ' ' || ask_search_suffix(options, answer), '((?:[A-Za-z0-9.-]+_){16})', '\1 ', 'g'))
  union all
  select 1 from issues where title ~ '(?:[A-Za-z0-9.-]+_){16}'
     and search is distinct from setweight(to_tsvector('english', key), 'A') || setweight(to_tsvector('english', regexp_replace(title, '((?:[A-Za-z0-9.-]+_){16})', '\1 ', 'g')), 'A')
) changed;
