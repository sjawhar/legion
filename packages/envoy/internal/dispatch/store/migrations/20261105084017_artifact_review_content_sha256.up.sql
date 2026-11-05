-- content_sha256 pins the stored UTF-8 version bytes; content_sha256_source says
-- whether they were hashed at insert or at a later backfill. Both are nullable so
-- the previous release can roll beside this one: its inserts are pinned by the
-- trigger, as 'insert', in the approving transaction. No rows are rewritten here:
-- a census counting a backfill would refuse the deploy. docs.FillReviewContentHashes
-- fills old rows at boot, marked 'backfill'. This migration's census answers 0.
create function artifact_version_text_sha256(artifact uuid, version integer)
returns text language sql stable parallel safe as $$
    select encode(sha256(convert_to(v.markdown, 'UTF8')), 'hex')
    from artifact_versions v where v.artifact_id = $1 and v.number = $2
$$;

alter table artifact_reviews add column content_sha256 text;
alter table artifact_reviews add column content_sha256_source text;
alter table artifact_reviews add constraint artifact_reviews_content_sha256_hex
    check (content_sha256 ~ '^[0-9a-f]{64}$') not valid;
alter table artifact_reviews add constraint artifact_reviews_content_sha256_source_check
    check ((content_sha256_source in ('insert', 'backfill') and content_sha256 is not null)
        or (content_sha256_source is null and content_sha256 is null)) not valid;

create function artifact_review_content_sha256_insert() returns trigger language plpgsql as $$
begin
    new.content_sha256 := artifact_version_text_sha256(new.artifact_id, new.version);
    new.content_sha256_source := case when new.content_sha256 is null then null else 'insert' end;
    return new;
end
$$;

create function artifact_review_content_sha256_update() returns trigger language plpgsql as $$
begin
    if old.content_sha256 is not null then
        if new.content_sha256 is distinct from old.content_sha256
            or new.content_sha256_source is distinct from old.content_sha256_source then
            raise exception 'artifact_reviews.content_sha256 is the hash of the approved bytes and never changes';
        end if;
    else
        new.content_sha256 := artifact_version_text_sha256(old.artifact_id, old.version);
        new.content_sha256_source := case when new.content_sha256 is null then null else 'backfill' end;
    end if;
    return new;
end
$$;

create trigger artifact_reviews_content_sha256_insert before insert on artifact_reviews
    for each row execute function artifact_review_content_sha256_insert();
create trigger artifact_reviews_content_sha256_update before update of content_sha256, content_sha256_source on artifact_reviews
    for each row execute function artifact_review_content_sha256_update();
