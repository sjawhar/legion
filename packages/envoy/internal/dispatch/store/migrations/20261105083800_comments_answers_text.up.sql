create index comments_answers_text on comments ((answers::text)) where answers is not null;
