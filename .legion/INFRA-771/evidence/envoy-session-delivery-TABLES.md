build: listener and bridge this checkout (fac9e5992 plus the working copy); plugins this checkout (fac9e5992 plus the working copy)
disruption             kind   event             marker           pi   cc
a session-restart      issue  comment-1         LDT8525-001Z      1    1
a session-restart      issue  comment-2         LDT8525-002Z      1    1
a session-restart      agent  dispatch-message  LDT8525-003Z      1    -
a session-restart      agent  dispatch-message  LDT8525-004Z      -    1
a session-restart      agent  listener-send     LDT8525-005Z      1    -
a session-restart      agent  listener-send     LDT8525-006Z      -    1
a session-restart      role   listener-publish  LDT8525-007Z      1    -
a session-restart      role   listener-publish  LDT8525-008Z      -    1
a session-restart      role   core-publish      LDT8525-009Z      1    -
a session-restart      role   core-publish      LDT8525-010Z      -    1
a session-restart      team   team-publish      LDT8525-011Z      1    1
b nats-cut             issue  comment-1         LDT8525-012Z      1    1
b nats-cut             issue  comment-2         LDT8525-013Z      1    1
b nats-cut             agent  dispatch-message  LDT8525-014Z      1    -
b nats-cut             agent  dispatch-message  LDT8525-015Z      -    1
b nats-cut             agent  listener-send     LDT8525-016Z      1    -
b nats-cut             agent  listener-send     LDT8525-017Z      -    1
b nats-cut             role   listener-publish  LDT8525-018Z      1    -
b nats-cut             role   listener-publish  LDT8525-019Z      -    1
b nats-cut             role   core-publish      LDT8525-020Z      1    -
b nats-cut             role   core-publish      LDT8525-021Z      -    1
b nats-cut             team   team-publish      LDT8525-022Z      1    1
c listener-shared      issue  comment-1         LDT8525-023Z      1    1
c listener-shared      issue  comment-2         LDT8525-024Z      1    1
c listener-shared      agent  dispatch-message  LDT8525-025Z      1    -
c listener-shared      agent  dispatch-message  LDT8525-026Z      -    1
c listener-shared      agent  listener-send     LDT8525-027Z      1    -
c listener-shared      agent  listener-send     LDT8525-028Z      -    1
c listener-shared      role   listener-publish  LDT8525-029Z      1    -
c listener-shared      role   listener-publish  LDT8525-030Z      -    1
c listener-shared      role   core-publish      LDT8525-031Z      1    -
c listener-shared      role   core-publish      LDT8525-032Z      -    1
c listener-shared      team   team-publish      LDT8525-033Z      1    1
d listener-dispatch    issue  comment-1         LDT8525-034Z      1    1
d listener-dispatch    issue  comment-2         LDT8525-035Z      1    1
d listener-dispatch    agent  dispatch-message  LDT8525-036Z    0/0    -
d listener-dispatch    agent  dispatch-message  LDT8525-037Z      -  0/0
d listener-dispatch    agent  listener-send     LDT8525-038Z      1    -
d listener-dispatch    agent  listener-send     LDT8525-039Z      -    1
d listener-dispatch    role   listener-publish  LDT8525-040Z      1    -
d listener-dispatch    role   listener-publish  LDT8525-041Z      -    1
d listener-dispatch    role   core-publish      LDT8525-042Z      1    -
d listener-dispatch    role   core-publish      LDT8525-043Z      -    1
d listener-dispatch    team   team-publish      LDT8525-044Z      1    1
e dispatch-kill        issue  comment-1         LDT8525-045Z      1    1
e dispatch-kill        issue  comment-2         LDT8525-046Z      1    1
e dispatch-kill        agent  dispatch-message  LDT8525-047Z      1    -
e dispatch-kill        agent  dispatch-message  LDT8525-048Z      -    1
e dispatch-kill        agent  listener-send     LDT8525-049Z      1    -
e dispatch-kill        agent  listener-send     LDT8525-050Z      -    1
e dispatch-kill        role   listener-publish  LDT8525-051Z      1    -
e dispatch-kill        role   listener-publish  LDT8525-052Z      -    1
e dispatch-kill        role   core-publish      LDT8525-053Z      1    -
e dispatch-kill        role   core-publish      LDT8525-054Z      -    1
e dispatch-kill        team   team-publish      LDT8525-055Z      1    1
f none                 issue  comment-1         LDT8525-056Z      1    1
f none                 issue  comment-2         LDT8525-057Z      1    1
f none                 agent  dispatch-message  LDT8525-058Z      1    -
f none                 agent  dispatch-message  LDT8525-059Z      -    1
f none                 agent  listener-send     LDT8525-060Z      1    -
f none                 agent  listener-send     LDT8525-061Z      -    1
f none                 role   listener-publish  LDT8525-062Z      1    -
f none                 role   listener-publish  LDT8525-063Z      -    1
f none                 role   core-publish      LDT8525-064Z      1    -
f none                 role   core-publish      LDT8525-065Z      -    1
f none                 team   team-publish      LDT8525-066Z      1    1
(a cell is the count of entries carrying the marker in that session's transcript; "!" marks a count other than expected; "/0" marks an event its publisher refused, expected 0 times; "-" an event not meant for that session)
note: Dispatch published LIVE-1's comments in another order than they were written: written LDT8525-001Z LDT8525-002Z LDT8525-012Z LDT8525-013Z LDT8525-023Z LDT8525-024Z LDT8525-034Z LDT8525-035Z LDT8525-045Z LDT8525-046Z LDT8525-056Z LDT8525-057Z ; stored LDT8525-001Z LDT8525-002Z LDT8525-012Z LDT8525-013Z LDT8525-023Z LDT8525-024Z LDT8525-034Z LDT8525-035Z LDT8525-046Z LDT8525-045Z LDT8525-056Z LDT8525-057Z
issue order compared in pi (12 comments): LDT8525-001Z@seq4:line17 LDT8525-002Z@seq5:line19 LDT8525-012Z@seq19:line40 LDT8525-013Z@seq20:line44 LDT8525-023Z@seq34:line63 LDT8525-024Z@seq35:line67 LDT8525-034Z@seq49:line86 LDT8525-035Z@seq50:line90 LDT8525-046Z@seq71:line127 LDT8525-045Z@seq76:line131 LDT8525-056Z@seq77:line135 LDT8525-057Z@seq78:line139
issue order compared in cc (12 comments): LDT8525-001Z@seq4:line62 LDT8525-002Z@seq5:line71 LDT8525-012Z@seq19:line132 LDT8525-013Z@seq20:line146 LDT8525-023Z@seq34:line205 LDT8525-024Z@seq35:line214 LDT8525-034Z@seq49:line280 LDT8525-035Z@seq50:line289 LDT8525-046Z@seq71:line391 LDT8525-045Z@seq76:line405 LDT8525-056Z@seq77:line414 LDT8525-057Z@seq78:line423
issue order: every comment of LIVE-1 appears in its stream order in both transcripts
transcripts: pi /legion/scratch/tmp/impl-round9/e2e/sessions/pi/sessions/2026-11-02T02-17-07-191Z_01a19a0b-cd77-7769-a7a1-0f57a3b7159f.jsonl; cc /legion/scratch/tmp/impl-round9/e2e/sessions/cc/config/projects/-legion-scratch-tmp-impl-round9-e2e-sessions-cc-cwd/9925e744-3049-4f3c-b408-d728dc21a940.jsonl
