build: listener and bridge this checkout (d5a918b13 plus the working copy); plugins this checkout (d5a918b13 plus the working copy)
disruption             kind   event             marker           pi   cc
a session-restart      issue  comment-1         LDTe98d-001Z      1    1
a session-restart      issue  comment-2         LDTe98d-002Z      1    1
a session-restart      agent  dispatch-message  LDTe98d-003Z      1    -
a session-restart      agent  dispatch-message  LDTe98d-004Z      -    1
a session-restart      agent  listener-send     LDTe98d-005Z      1    -
a session-restart      agent  listener-send     LDTe98d-006Z      -    1
a session-restart      role   listener-publish  LDTe98d-007Z      1    -
a session-restart      role   listener-publish  LDTe98d-008Z      -    1
a session-restart      role   core-publish      LDTe98d-009Z      1    -
a session-restart      role   core-publish      LDTe98d-010Z      -    1
a session-restart      team   team-publish      LDTe98d-011Z      1    1
b nats-cut             issue  comment-1         LDTe98d-012Z      1    1
b nats-cut             issue  comment-2         LDTe98d-013Z      1    1
b nats-cut             agent  dispatch-message  LDTe98d-014Z      1    -
b nats-cut             agent  dispatch-message  LDTe98d-015Z      -    1
b nats-cut             agent  listener-send     LDTe98d-016Z      1    -
b nats-cut             agent  listener-send     LDTe98d-017Z      -    1
b nats-cut             role   listener-publish  LDTe98d-018Z      1    -
b nats-cut             role   listener-publish  LDTe98d-019Z      -    1
b nats-cut             role   core-publish      LDTe98d-020Z      1    -
b nats-cut             role   core-publish      LDTe98d-021Z      -    1
b nats-cut             team   team-publish      LDTe98d-022Z      1    1
c listener-shared      issue  comment-1         LDTe98d-023Z      1    1
c listener-shared      issue  comment-2         LDTe98d-024Z      1    1
c listener-shared      agent  dispatch-message  LDTe98d-025Z      1    -
c listener-shared      agent  dispatch-message  LDTe98d-026Z      -    1
c listener-shared      agent  listener-send     LDTe98d-027Z      1    -
c listener-shared      agent  listener-send     LDTe98d-028Z      -    1
c listener-shared      role   listener-publish  LDTe98d-029Z      1    -
c listener-shared      role   listener-publish  LDTe98d-030Z      -    1
c listener-shared      role   core-publish      LDTe98d-031Z      1    -
c listener-shared      role   core-publish      LDTe98d-032Z      -    1
c listener-shared      team   team-publish      LDTe98d-033Z      1    1
d listener-dispatch    issue  comment-1         LDTe98d-034Z      1    1
d listener-dispatch    issue  comment-2         LDTe98d-035Z      1    1
d listener-dispatch    agent  dispatch-message  LDTe98d-036Z    0/0    -
d listener-dispatch    agent  dispatch-message  LDTe98d-037Z      -  0/0
d listener-dispatch    agent  listener-send     LDTe98d-038Z      1    -
d listener-dispatch    agent  listener-send     LDTe98d-039Z      -    1
d listener-dispatch    role   listener-publish  LDTe98d-040Z      1    -
d listener-dispatch    role   listener-publish  LDTe98d-041Z      -    1
d listener-dispatch    role   core-publish      LDTe98d-042Z      1    -
d listener-dispatch    role   core-publish      LDTe98d-043Z      -    1
d listener-dispatch    team   team-publish      LDTe98d-044Z      1    1
e dispatch-kill        issue  comment-1         LDTe98d-045Z      1    1
e dispatch-kill        issue  comment-2         LDTe98d-046Z      1    1
e dispatch-kill        agent  dispatch-message  LDTe98d-047Z      1    -
e dispatch-kill        agent  dispatch-message  LDTe98d-048Z      -    1
e dispatch-kill        agent  listener-send     LDTe98d-049Z      1    -
e dispatch-kill        agent  listener-send     LDTe98d-050Z      -    1
e dispatch-kill        role   listener-publish  LDTe98d-051Z      1    -
e dispatch-kill        role   listener-publish  LDTe98d-052Z      -    1
e dispatch-kill        role   core-publish      LDTe98d-053Z      1    -
e dispatch-kill        role   core-publish      LDTe98d-054Z      -    1
e dispatch-kill        team   team-publish      LDTe98d-055Z      1    1
f none                 issue  comment-1         LDTe98d-056Z      1    1
f none                 issue  comment-2         LDTe98d-057Z      1    1
f none                 agent  dispatch-message  LDTe98d-058Z      1    -
f none                 agent  dispatch-message  LDTe98d-059Z      -    1
f none                 agent  listener-send     LDTe98d-060Z      1    -
f none                 agent  listener-send     LDTe98d-061Z      -    1
f none                 role   listener-publish  LDTe98d-062Z      1    -
f none                 role   listener-publish  LDTe98d-063Z      -    1
f none                 role   core-publish      LDTe98d-064Z      1    -
f none                 role   core-publish      LDTe98d-065Z      -    1
f none                 team   team-publish      LDTe98d-066Z      1    1
(a cell is the count of entries carrying the marker in that session's transcript; "!" marks a count other than expected; "/0" marks an event its publisher refused, expected 0 times; "-" an event not meant for that session)
note: Dispatch published LIVE-1's comments in another order than they were written: written LDTe98d-001Z LDTe98d-002Z LDTe98d-012Z LDTe98d-013Z LDTe98d-023Z LDTe98d-024Z LDTe98d-034Z LDTe98d-035Z LDTe98d-045Z LDTe98d-046Z LDTe98d-056Z LDTe98d-057Z ; stored LDTe98d-001Z LDTe98d-002Z LDTe98d-012Z LDTe98d-013Z LDTe98d-023Z LDTe98d-024Z LDTe98d-034Z LDTe98d-035Z LDTe98d-046Z LDTe98d-045Z LDTe98d-056Z LDTe98d-057Z
issue order compared in pi (12 comments): LDTe98d-001Z@seq4:line17 LDTe98d-002Z@seq5:line18 LDTe98d-012Z@seq19:line40 LDTe98d-013Z@seq20:line44 LDTe98d-023Z@seq34:line63 LDTe98d-024Z@seq35:line67 LDTe98d-034Z@seq49:line86 LDTe98d-035Z@seq50:line90 LDTe98d-046Z@seq71:line126 LDTe98d-045Z@seq76:line130 LDTe98d-056Z@seq77:line134 LDTe98d-057Z@seq78:line138
issue order compared in cc (12 comments): LDTe98d-001Z@seq4:line62 LDTe98d-002Z@seq5:line71 LDTe98d-012Z@seq19:line132 LDTe98d-013Z@seq20:line146 LDTe98d-023Z@seq34:line205 LDTe98d-024Z@seq35:line214 LDTe98d-034Z@seq49:line280 LDTe98d-035Z@seq50:line289 LDTe98d-046Z@seq71:line391 LDTe98d-045Z@seq76:line405 LDTe98d-056Z@seq77:line414 LDTe98d-057Z@seq78:line423
issue order: every comment of LIVE-1 appears in its stream order in both transcripts
transcripts: pi /legion/scratch/tmp/impl-round7/e2e/sessions/pi/sessions/2026-11-01T18-59-47-646Z_01a1987b-6b3e-7656-a67a-22695f46ec16.jsonl; cc /legion/scratch/tmp/impl-round7/e2e/sessions/cc/config/projects/-legion-scratch-tmp-impl-round7-e2e-sessions-cc-cwd/aebb97f6-cc55-4af4-aafd-71de437a6bd1.jsonl
