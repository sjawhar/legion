-- 0007_ask_resolution.up.sql
alter table asks add column resolution jsonb;
alter table asks add constraint asks_state_check check (state in ('open', 'answered', 'resolved'));
alter table asks add constraint asks_resolution_state_check check ((state = 'resolved') = (resolution is not null));
alter table asks add constraint asks_answer_state_check check ((state = 'answered') = (answer is not null));
