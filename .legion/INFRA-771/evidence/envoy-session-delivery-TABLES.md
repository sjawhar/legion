build: listener and bridge this checkout (306ad1b10 plus the working copy); plugins this checkout (306ad1b10 plus the working copy)
disruption             kind   event             marker           pi   cc
a session-restart      issue  comment-1         LDT5e01-001Z      1    1
a session-restart      issue  comment-2         LDT5e01-002Z      1    1
a session-restart      agent  dispatch-message  LDT5e01-003Z      1    -
a session-restart      agent  dispatch-message  LDT5e01-004Z      -    1
a session-restart      agent  listener-send     LDT5e01-005Z      1    -
a session-restart      agent  listener-send     LDT5e01-006Z      -    1
a session-restart      role   listener-publish  LDT5e01-007Z      1    -
a session-restart      role   listener-publish  LDT5e01-008Z      -    1
a session-restart      role   core-publish      LDT5e01-009Z      1    -
a session-restart      role   core-publish      LDT5e01-010Z      -    1
a session-restart      team   team-publish      LDT5e01-011Z      1    1
b nats-cut             issue  comment-1         LDT5e01-012Z      1    1
b nats-cut             issue  comment-2         LDT5e01-013Z      1    1
b nats-cut             agent  dispatch-message  LDT5e01-014Z      1    -
b nats-cut             agent  dispatch-message  LDT5e01-015Z      -    1
b nats-cut             agent  listener-send     LDT5e01-016Z      1    -
b nats-cut             agent  listener-send     LDT5e01-017Z      -    1
b nats-cut             role   listener-publish  LDT5e01-018Z      1    -
b nats-cut             role   listener-publish  LDT5e01-019Z      -    1
b nats-cut             role   core-publish      LDT5e01-020Z      1    -
b nats-cut             role   core-publish      LDT5e01-021Z      -    1
b nats-cut             team   team-publish      LDT5e01-022Z      1    1
c listener-shared      issue  comment-1         LDT5e01-023Z      1    1
c listener-shared      issue  comment-2         LDT5e01-024Z      1    1
c listener-shared      agent  dispatch-message  LDT5e01-025Z      1    -
c listener-shared      agent  dispatch-message  LDT5e01-026Z      -    1
c listener-shared      agent  listener-send     LDT5e01-027Z      1    -
c listener-shared      agent  listener-send     LDT5e01-028Z      -    1
c listener-shared      role   listener-publish  LDT5e01-029Z      1    -
c listener-shared      role   listener-publish  LDT5e01-030Z      -    1
c listener-shared      role   core-publish      LDT5e01-031Z      1    -
c listener-shared      role   core-publish      LDT5e01-032Z      -    1
c listener-shared      team   team-publish      LDT5e01-033Z      1    1
d listener-dispatch    issue  comment-1         LDT5e01-034Z      1    1
d listener-dispatch    issue  comment-2         LDT5e01-035Z      1    1
d listener-dispatch    agent  dispatch-message  LDT5e01-036Z    0/0    -
d listener-dispatch    agent  dispatch-message  LDT5e01-037Z      -  0/0
d listener-dispatch    agent  listener-send     LDT5e01-038Z      1    -
d listener-dispatch    agent  listener-send     LDT5e01-039Z      -    1
d listener-dispatch    role   listener-publish  LDT5e01-040Z      1    -
d listener-dispatch    role   listener-publish  LDT5e01-041Z      -    1
d listener-dispatch    role   core-publish      LDT5e01-042Z      1    -
d listener-dispatch    role   core-publish      LDT5e01-043Z      -    1
d listener-dispatch    team   team-publish      LDT5e01-044Z      1    1
e dispatch-kill        issue  comment-1         LDT5e01-045Z      1    1
e dispatch-kill        issue  comment-2         LDT5e01-046Z      1    1
e dispatch-kill        agent  dispatch-message  LDT5e01-047Z      1    -
e dispatch-kill        agent  dispatch-message  LDT5e01-048Z      -    1
e dispatch-kill        agent  listener-send     LDT5e01-049Z      1    -
e dispatch-kill        agent  listener-send     LDT5e01-050Z      -    1
e dispatch-kill        role   listener-publish  LDT5e01-051Z      1    -
e dispatch-kill        role   listener-publish  LDT5e01-052Z      -    1
e dispatch-kill        role   core-publish      LDT5e01-053Z      1    -
e dispatch-kill        role   core-publish      LDT5e01-054Z      -    1
e dispatch-kill        team   team-publish      LDT5e01-055Z      1    1
f none                 issue  comment-1         LDT5e01-056Z      1    1
f none                 issue  comment-2         LDT5e01-057Z      1    1
f none                 agent  dispatch-message  LDT5e01-058Z      1    -
f none                 agent  dispatch-message  LDT5e01-059Z      -    1
f none                 agent  listener-send     LDT5e01-060Z      1    -
f none                 agent  listener-send     LDT5e01-061Z      -    1
f none                 role   listener-publish  LDT5e01-062Z      1    -
f none                 role   listener-publish  LDT5e01-063Z      -    1
f none                 role   core-publish      LDT5e01-064Z      1    -
f none                 role   core-publish      LDT5e01-065Z      -    1
f none                 team   team-publish      LDT5e01-066Z      1    1
(a cell is the count of entries carrying the marker in that session's transcript; "!" marks a count other than expected; "/0" marks an event its publisher refused, expected 0 times; "-" an event not meant for that session)
note: Dispatch published LIVE-1's comments in another order than they were written: written LDT5e01-001Z LDT5e01-002Z LDT5e01-012Z LDT5e01-013Z LDT5e01-023Z LDT5e01-024Z LDT5e01-034Z LDT5e01-035Z LDT5e01-045Z LDT5e01-046Z LDT5e01-056Z LDT5e01-057Z ; stored LDT5e01-001Z LDT5e01-002Z LDT5e01-012Z LDT5e01-013Z LDT5e01-023Z LDT5e01-024Z LDT5e01-034Z LDT5e01-035Z LDT5e01-046Z LDT5e01-045Z LDT5e01-056Z LDT5e01-057Z
issue order compared in pi (12 comments): LDT5e01-001Z@seq4:line17 LDT5e01-002Z@seq5:line18 LDT5e01-012Z@seq19:line40 LDT5e01-013Z@seq20:line44 LDT5e01-023Z@seq34:line63 LDT5e01-024Z@seq35:line67 LDT5e01-034Z@seq49:line86 LDT5e01-035Z@seq50:line90 LDT5e01-046Z@seq71:line127 LDT5e01-045Z@seq76:line131 LDT5e01-056Z@seq77:line135 LDT5e01-057Z@seq78:line139
issue order compared in cc (12 comments): LDT5e01-001Z@seq4:line58 LDT5e01-002Z@seq5:line67 LDT5e01-012Z@seq19:line128 LDT5e01-013Z@seq20:line142 LDT5e01-023Z@seq34:line201 LDT5e01-024Z@seq35:line210 LDT5e01-034Z@seq49:line276 LDT5e01-035Z@seq50:line285 LDT5e01-046Z@seq71:line387 LDT5e01-045Z@seq76:line401 LDT5e01-056Z@seq77:line410 LDT5e01-057Z@seq78:line419
issue order: every comment of LIVE-1 appears in its stream order in both transcripts
transcripts: pi /legion/scratch/tmp/impl-round6/sessions/pi/sessions/2026-11-01T06-38-30-108Z_01a195d4-bedc-7656-bfbd-42d1808d2a92.jsonl; cc /legion/scratch/tmp/impl-round6/sessions/cc/config/projects/-legion-scratch-tmp-impl-round6-sessions-cc-cwd/3f42dd84-3c42-4459-b2f9-5e238a9b6dbc.jsonl
