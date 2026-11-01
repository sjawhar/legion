build: listener and bridge this checkout (77e134dcb plus the working copy); plugins this checkout (77e134dcb plus the working copy)
disruption             kind   event             marker           pi   cc
a session-restart      issue  comment-1         LDT6c99-001Z      1    1
a session-restart      issue  comment-2         LDT6c99-002Z      1    1
a session-restart      agent  dispatch-message  LDT6c99-003Z      1    -
a session-restart      agent  dispatch-message  LDT6c99-004Z      -    1
a session-restart      agent  listener-send     LDT6c99-005Z      1    -
a session-restart      agent  listener-send     LDT6c99-006Z      -    1
a session-restart      role   listener-publish  LDT6c99-007Z      1    -
a session-restart      role   listener-publish  LDT6c99-008Z      -    1
a session-restart      role   core-publish      LDT6c99-009Z      1    -
a session-restart      role   core-publish      LDT6c99-010Z      -    1
a session-restart      team   team-publish      LDT6c99-011Z      1    1
b nats-cut             issue  comment-1         LDT6c99-012Z      1    1
b nats-cut             issue  comment-2         LDT6c99-013Z      1    1
b nats-cut             agent  dispatch-message  LDT6c99-014Z      1    -
b nats-cut             agent  dispatch-message  LDT6c99-015Z      -    1
b nats-cut             agent  listener-send     LDT6c99-016Z      1    -
b nats-cut             agent  listener-send     LDT6c99-017Z      -    1
b nats-cut             role   listener-publish  LDT6c99-018Z      1    -
b nats-cut             role   listener-publish  LDT6c99-019Z      -    1
b nats-cut             role   core-publish      LDT6c99-020Z      1    -
b nats-cut             role   core-publish      LDT6c99-021Z      -    1
b nats-cut             team   team-publish      LDT6c99-022Z      1    1
c listener-shared      issue  comment-1         LDT6c99-023Z      1    1
c listener-shared      issue  comment-2         LDT6c99-024Z      1    1
c listener-shared      agent  dispatch-message  LDT6c99-025Z      1    -
c listener-shared      agent  dispatch-message  LDT6c99-026Z      -    1
c listener-shared      agent  listener-send     LDT6c99-027Z      1    -
c listener-shared      agent  listener-send     LDT6c99-028Z      -    1
c listener-shared      role   listener-publish  LDT6c99-029Z      1    -
c listener-shared      role   listener-publish  LDT6c99-030Z      -    1
c listener-shared      role   core-publish      LDT6c99-031Z      1    -
c listener-shared      role   core-publish      LDT6c99-032Z      -    1
c listener-shared      team   team-publish      LDT6c99-033Z      1    1
d listener-dispatch    issue  comment-1         LDT6c99-034Z      1    1
d listener-dispatch    issue  comment-2         LDT6c99-035Z      1    1
d listener-dispatch    agent  dispatch-message  LDT6c99-036Z    0/0    -
d listener-dispatch    agent  dispatch-message  LDT6c99-037Z      -  0/0
d listener-dispatch    agent  listener-send     LDT6c99-038Z      1    -
d listener-dispatch    agent  listener-send     LDT6c99-039Z      -    1
d listener-dispatch    role   listener-publish  LDT6c99-040Z      1    -
d listener-dispatch    role   listener-publish  LDT6c99-041Z      -    1
d listener-dispatch    role   core-publish      LDT6c99-042Z      1    -
d listener-dispatch    role   core-publish      LDT6c99-043Z      -    1
d listener-dispatch    team   team-publish      LDT6c99-044Z      1    1
e dispatch-kill        issue  comment-1         LDT6c99-045Z      1    1
e dispatch-kill        issue  comment-2         LDT6c99-046Z      1    1
e dispatch-kill        agent  dispatch-message  LDT6c99-047Z      1    -
e dispatch-kill        agent  dispatch-message  LDT6c99-048Z      -    1
e dispatch-kill        agent  listener-send     LDT6c99-049Z      1    -
e dispatch-kill        agent  listener-send     LDT6c99-050Z      -    1
e dispatch-kill        role   listener-publish  LDT6c99-051Z      1    -
e dispatch-kill        role   listener-publish  LDT6c99-052Z      -    1
e dispatch-kill        role   core-publish      LDT6c99-053Z      1    -
e dispatch-kill        role   core-publish      LDT6c99-054Z      -    1
e dispatch-kill        team   team-publish      LDT6c99-055Z      1    1
f none                 issue  comment-1         LDT6c99-056Z      1    1
f none                 issue  comment-2         LDT6c99-057Z      1    1
f none                 agent  dispatch-message  LDT6c99-058Z      1    -
f none                 agent  dispatch-message  LDT6c99-059Z      -    1
f none                 agent  listener-send     LDT6c99-060Z      1    -
f none                 agent  listener-send     LDT6c99-061Z      -    1
f none                 role   listener-publish  LDT6c99-062Z      1    -
f none                 role   listener-publish  LDT6c99-063Z      -    1
f none                 role   core-publish      LDT6c99-064Z      1    -
f none                 role   core-publish      LDT6c99-065Z      -    1
f none                 team   team-publish      LDT6c99-066Z      1    1
(a cell is the count of entries carrying the marker in that session's transcript; "!" marks a count other than expected; "/0" marks an event its publisher refused, expected 0 times; "-" an event not meant for that session)
note: Dispatch published LIVE-1's comments in another order than they were written: written LDT6c99-001Z LDT6c99-002Z LDT6c99-012Z LDT6c99-013Z LDT6c99-023Z LDT6c99-024Z LDT6c99-034Z LDT6c99-035Z LDT6c99-045Z LDT6c99-046Z LDT6c99-056Z LDT6c99-057Z ; stored LDT6c99-001Z LDT6c99-002Z LDT6c99-012Z LDT6c99-013Z LDT6c99-023Z LDT6c99-024Z LDT6c99-034Z LDT6c99-035Z LDT6c99-046Z LDT6c99-045Z LDT6c99-056Z LDT6c99-057Z
issue order compared in pi (12 comments): LDT6c99-001Z@seq4:line17 LDT6c99-002Z@seq5:line18 LDT6c99-012Z@seq19:line40 LDT6c99-013Z@seq20:line44 LDT6c99-023Z@seq34:line63 LDT6c99-024Z@seq35:line67 LDT6c99-034Z@seq49:line86 LDT6c99-035Z@seq50:line90 LDT6c99-046Z@seq71:line127 LDT6c99-045Z@seq76:line131 LDT6c99-056Z@seq77:line135 LDT6c99-057Z@seq78:line139
issue order compared in cc (12 comments): LDT6c99-001Z@seq4:line62 LDT6c99-002Z@seq5:line71 LDT6c99-012Z@seq19:line132 LDT6c99-013Z@seq20:line146 LDT6c99-023Z@seq34:line205 LDT6c99-024Z@seq35:line214 LDT6c99-034Z@seq49:line280 LDT6c99-035Z@seq50:line289 LDT6c99-046Z@seq71:line391 LDT6c99-045Z@seq76:line405 LDT6c99-056Z@seq77:line414 LDT6c99-057Z@seq78:line423
issue order: every comment of LIVE-1 appears in its stream order in both transcripts
transcripts: pi /legion/scratch/tmp/impl-round8/e2e/sessions/pi/sessions/2026-11-01T23-46-38-576Z_01a19982-0970-76b4-97f7-1a8b7960f930.jsonl; cc /legion/scratch/tmp/impl-round8/e2e/sessions/cc/config/projects/-legion-scratch-tmp-impl-round8-e2e-sessions-cc-cwd/42c16a0e-1e52-41d9-b0a8-3493d9305134.jsonl
