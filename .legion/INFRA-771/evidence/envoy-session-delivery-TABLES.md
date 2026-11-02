build: listener and bridge this checkout (c01d14e96 plus the working copy); plugins this checkout (c01d14e96 plus the working copy)
disruption             kind   event             marker           pi   cc
a session-restart      issue  comment-1         LDTa216-001Z      1    1
a session-restart      issue  comment-2         LDTa216-002Z      1    1
a session-restart      agent  dispatch-message  LDTa216-003Z      1    -
a session-restart      agent  dispatch-message  LDTa216-004Z      -    1
a session-restart      agent  listener-send     LDTa216-005Z      1    -
a session-restart      agent  listener-send     LDTa216-006Z      -    1
a session-restart      role   listener-publish  LDTa216-007Z      1    -
a session-restart      role   listener-publish  LDTa216-008Z      -    1
a session-restart      role   core-publish      LDTa216-009Z      1    -
a session-restart      role   core-publish      LDTa216-010Z      -    1
a session-restart      team   team-publish      LDTa216-011Z      1    1
b nats-cut             issue  comment-1         LDTa216-012Z      1    1
b nats-cut             issue  comment-2         LDTa216-013Z      1    1
b nats-cut             agent  dispatch-message  LDTa216-014Z      1    -
b nats-cut             agent  dispatch-message  LDTa216-015Z      -    1
b nats-cut             agent  listener-send     LDTa216-016Z      1    -
b nats-cut             agent  listener-send     LDTa216-017Z      -    1
b nats-cut             role   listener-publish  LDTa216-018Z      1    -
b nats-cut             role   listener-publish  LDTa216-019Z      -    1
b nats-cut             role   core-publish      LDTa216-020Z      1    -
b nats-cut             role   core-publish      LDTa216-021Z      -    1
b nats-cut             team   team-publish      LDTa216-022Z      1    1
c listener-shared      issue  comment-1         LDTa216-023Z      1    1
c listener-shared      issue  comment-2         LDTa216-024Z      1    1
c listener-shared      agent  dispatch-message  LDTa216-025Z      1    -
c listener-shared      agent  dispatch-message  LDTa216-026Z      -    1
c listener-shared      agent  listener-send     LDTa216-027Z      1    -
c listener-shared      agent  listener-send     LDTa216-028Z      -    1
c listener-shared      role   listener-publish  LDTa216-029Z      1    -
c listener-shared      role   listener-publish  LDTa216-030Z      -    1
c listener-shared      role   core-publish      LDTa216-031Z      1    -
c listener-shared      role   core-publish      LDTa216-032Z      -    1
c listener-shared      team   team-publish      LDTa216-033Z      1    1
d listener-dispatch    issue  comment-1         LDTa216-034Z      1    1
d listener-dispatch    issue  comment-2         LDTa216-035Z      1    1
d listener-dispatch    agent  dispatch-message  LDTa216-036Z    0/0    -
d listener-dispatch    agent  dispatch-message  LDTa216-037Z      -  0/0
d listener-dispatch    agent  listener-send     LDTa216-038Z      1    -
d listener-dispatch    agent  listener-send     LDTa216-039Z      -    1
d listener-dispatch    role   listener-publish  LDTa216-040Z      1    -
d listener-dispatch    role   listener-publish  LDTa216-041Z      -    1
d listener-dispatch    role   core-publish      LDTa216-042Z      1    -
d listener-dispatch    role   core-publish      LDTa216-043Z      -    1
d listener-dispatch    team   team-publish      LDTa216-044Z      1    1
e dispatch-kill        issue  comment-1         LDTa216-045Z      1    1
e dispatch-kill        issue  comment-2         LDTa216-046Z      1    1
e dispatch-kill        agent  dispatch-message  LDTa216-047Z      1    -
e dispatch-kill        agent  dispatch-message  LDTa216-048Z      -    1
e dispatch-kill        agent  listener-send     LDTa216-049Z      1    -
e dispatch-kill        agent  listener-send     LDTa216-050Z      -    1
e dispatch-kill        role   listener-publish  LDTa216-051Z      1    -
e dispatch-kill        role   listener-publish  LDTa216-052Z      -    1
e dispatch-kill        role   core-publish      LDTa216-053Z      1    -
e dispatch-kill        role   core-publish      LDTa216-054Z      -    1
e dispatch-kill        team   team-publish      LDTa216-055Z      1    1
f none                 issue  comment-1         LDTa216-056Z      1    1
f none                 issue  comment-2         LDTa216-057Z      1    1
f none                 agent  dispatch-message  LDTa216-058Z      1    -
f none                 agent  dispatch-message  LDTa216-059Z      -    1
f none                 agent  listener-send     LDTa216-060Z      1    -
f none                 agent  listener-send     LDTa216-061Z      -    1
f none                 role   listener-publish  LDTa216-062Z      1    -
f none                 role   listener-publish  LDTa216-063Z      -    1
f none                 role   core-publish      LDTa216-064Z      1    -
f none                 role   core-publish      LDTa216-065Z      -    1
f none                 team   team-publish      LDTa216-066Z      1    1
(a cell is the count of entries carrying the marker in that session's transcript; "!" marks a count other than expected; "/0" marks an event its publisher refused, expected 0 times; "-" an event not meant for that session)
issue order compared in pi (12 comments): LDTa216-001Z@seq4:line17 LDTa216-002Z@seq5:line19 LDTa216-012Z@seq19:line40 LDTa216-013Z@seq20:line44 LDTa216-023Z@seq34:line63 LDTa216-024Z@seq35:line67 LDTa216-034Z@seq49:line86 LDTa216-035Z@seq50:line90 LDTa216-045Z@seq71:line127 LDTa216-046Z@seq74:line131 LDTa216-056Z@seq77:line135 LDTa216-057Z@seq78:line139
issue order compared in cc (12 comments): LDTa216-001Z@seq4:line59 LDTa216-002Z@seq5:line68 LDTa216-012Z@seq19:line129 LDTa216-013Z@seq20:line143 LDTa216-023Z@seq34:line202 LDTa216-024Z@seq35:line211 LDTa216-034Z@seq49:line277 LDTa216-035Z@seq50:line286 LDTa216-045Z@seq71:line388 LDTa216-046Z@seq74:line402 LDTa216-056Z@seq77:line411 LDTa216-057Z@seq78:line420
issue order: every comment of LIVE-1 appears in its stream order in both transcripts
transcripts: pi /legion/scratch/tmp/impl-round10b/e2e/sessions/pi/sessions/2026-11-02T05-12-15-855Z_01a19aac-26ef-7714-b1b4-92bcb9f3ae93.jsonl; cc /legion/scratch/tmp/impl-round10b/e2e/sessions/cc/config/projects/-legion-scratch-tmp-impl-round10b-e2e-sessions-cc-cwd/26747ffb-19a6-41d7-8c49-a9af9d774ab3.jsonl
