-- Read-only: the summary and its stable hand-check sample use the same classification.
with requirements as (
    select a.issue_key, b.node_id, latest.text
    from artifacts a
    join document_blocks b on b.artifact_id = a.id and b.type = 'requirement'
    left join document_block_versions v on v.artifact_id = a.id
    left join lateral (
        select t.text
        from document_block_texts t
        where t.artifact_id = b.artifact_id and t.block_id = b.block_id
          and t.version <= v.version and t.text is not null
        order by t.version desc
        limit 1
    ) latest on true
    where a.is_primary and a.kind = 'doc'
), scope as (
    select distinct issue_key from requirements
), citations as (
    select r.node_id, u.url,
           case
               when u.url ~ '/(ask|comment|message)/' then 'other'
               when exists (
                   select 1 from document_blocks target
                   where target.node_id = regexp_replace(u.url,
                       '^dispatch://([^/]+)/(?:artifact/)?([^#]+)#b-(.+)$', '\1/\2#\3')
                     and u.url ~ '^dispatch://[^/]+/(spec|artifact/[^#]+)#b-'
                     and target.type = 'ask'
               ) then 'document_ask'
           end as source
    from requirements r
    cross join lateral regexp_matches(r.text, 'dispatch://[^\s)>\]"]+', 'g') matched
    cross join lateral (select matched[1] as url) u
), classified as (
    select node_id,
           bool_or(source = 'document_ask') as document_ask,
           bool_or(source = 'other') as other
    from citations
    where source is not null
    group by node_id
), person_answers as (
    select a.block_artifact_id
    from asks a
    left join artifacts owner on owner.id = a.artifact_id
    join scope s on s.issue_key = coalesce(a.issue_key, owner.issue_key)
    join lateral (
        select e.actor
        from events e
        where e.type = 'ask.answered' and e.payload->>'id' = a.id::text
          and (e.issue_key = a.issue_key or e.artifact_id = a.artifact_id)
        order by e.id desc
        limit 1
    ) answered on answered.actor->>'kind' = 'user'
    where a.answer is not null
), counts as (
    select (select count(*) from scope) as scope_issues,
           (select count(*) from requirements) as requirement_blocks,
           (select count(*) from classified) as cited_requirement_blocks,
           (select count(*) from classified where document_ask and not other) as cited_document_ask_only,
           (select count(*) from classified where other and not document_ask) as cited_other_only,
           (select count(*) from classified where document_ask and other) as cited_both,
           (select count(*) from person_answers where block_artifact_id is not null) as person_document_ask_answers,
           (select count(*) from person_answers where block_artifact_id is null) as person_item_ask_answers,
           (select count(*) from comments c
            left join artifacts a on a.id = c.artifact_id and a.kind = 'doc'
            join scope s on s.issue_key = coalesce(c.issue_key, a.issue_key)
            where c.author->>'kind' = 'user') as person_comments,
           (select count(*) from messages m join scope s on s.issue_key = m.issue_key
            where m.author->>'kind' = 'user') as person_messages
)
select counts.*,
       round(cited_other_only::numeric / nullif(cited_requirement_blocks, 0), 3) as item24_share_cited,
       round((person_item_ask_answers + person_comments + person_messages)::numeric /
             nullif(person_document_ask_answers + person_item_ask_answers + person_comments + person_messages, 0), 3) as item24_share_volume
from counts;

with requirements as (
    select b.node_id, latest.text
    from artifacts a
    join document_blocks b on b.artifact_id = a.id and b.type = 'requirement'
    left join document_block_versions v on v.artifact_id = a.id
    left join lateral (
        select t.text
        from document_block_texts t
        where t.artifact_id = b.artifact_id and t.block_id = b.block_id
          and t.version <= v.version and t.text is not null
        order by t.version desc
        limit 1
    ) latest on true
    where a.is_primary and a.kind = 'doc'
), citations as (
    select r.node_id, u.url,
           case
               when u.url ~ '/(ask|comment|message)/' then 'other'
               when exists (
                   select 1 from document_blocks target
                   where target.node_id = regexp_replace(u.url,
                       '^dispatch://([^/]+)/(?:artifact/)?([^#]+)#b-(.+)$', '\1/\2#\3')
                     and u.url ~ '^dispatch://[^/]+/(spec|artifact/[^#]+)#b-'
                     and target.type = 'ask'
               ) then 'document_ask'
           end as source
    from requirements r
    cross join lateral regexp_matches(r.text, 'dispatch://[^\s)>\]"]+', 'g') matched
    cross join lateral (select matched[1] as url) u
), classified as (
    select node_id,
           bool_or(source = 'document_ask') as document_ask,
           bool_or(source = 'other') as other,
           string_agg(distinct url, '; ' order by url) as urls
    from citations
    where source is not null
    group by node_id
)
select node_id,
       case when document_ask and other then 'both'
            when document_ask then 'document_ask_only'
            else 'other_only' end as class,
       urls
from classified
order by md5(node_id)
limit 10;
