build: listener and bridge this checkout (f9ea0087e plus the working copy); plugins this checkout (f9ea0087e plus the working copy)
disruption             kind   event             marker           pi   cc
a session-restart      issue  comment-1         LDT3a18-001Z      1    1
a session-restart      issue  comment-2         LDT3a18-002Z      1    1
a session-restart      agent  dispatch-message  LDT3a18-003Z      1    -
a session-restart      agent  dispatch-message  LDT3a18-004Z      -    1
a session-restart      agent  listener-send     LDT3a18-005Z      1    -
a session-restart      agent  listener-send     LDT3a18-006Z      -    1
a session-restart      role   listener-publish  LDT3a18-007Z      1    -
a session-restart      role   listener-publish  LDT3a18-008Z      -    1
a session-restart      role   core-publish      LDT3a18-009Z      1    -
a session-restart      role   core-publish      LDT3a18-010Z      -    1
a session-restart      team   team-publish      LDT3a18-011Z      1    1
b nats-cut             issue  comment-1         LDT3a18-012Z      1    1
b nats-cut             issue  comment-2         LDT3a18-013Z      1    1
b nats-cut             agent  dispatch-message  LDT3a18-014Z      1    -
b nats-cut             agent  dispatch-message  LDT3a18-015Z      -    1
b nats-cut             agent  listener-send     LDT3a18-016Z      1    -
b nats-cut             agent  listener-send     LDT3a18-017Z      -    1
b nats-cut             role   listener-publish  LDT3a18-018Z      1    -
b nats-cut             role   listener-publish  LDT3a18-019Z      -    1
b nats-cut             role   core-publish      LDT3a18-020Z      1    -
b nats-cut             role   core-publish      LDT3a18-021Z      -    1
b nats-cut             team   team-publish      LDT3a18-022Z      1    1
c listener-shared      issue  comment-1         LDT3a18-023Z      1    1
c listener-shared      issue  comment-2         LDT3a18-024Z      1    1
c listener-shared      agent  dispatch-message  LDT3a18-025Z      1    -
c listener-shared      agent  dispatch-message  LDT3a18-026Z      -    1
c listener-shared      agent  listener-send     LDT3a18-027Z      1    -
c listener-shared      agent  listener-send     LDT3a18-028Z      -    1
c listener-shared      role   listener-publish  LDT3a18-029Z      1    -
c listener-shared      role   listener-publish  LDT3a18-030Z      -    1
c listener-shared      role   core-publish      LDT3a18-031Z      1    -
c listener-shared      role   core-publish      LDT3a18-032Z      -    1
c listener-shared      team   team-publish      LDT3a18-033Z      1    1
d listener-dispatch    issue  comment-1         LDT3a18-034Z      1    1
d listener-dispatch    issue  comment-2         LDT3a18-035Z      1    1
d listener-dispatch    agent  dispatch-message  LDT3a18-036Z    0/0    -
d listener-dispatch    agent  dispatch-message  LDT3a18-037Z      -  0/0
d listener-dispatch    agent  listener-send     LDT3a18-038Z      1    -
d listener-dispatch    agent  listener-send     LDT3a18-039Z      -    1
d listener-dispatch    role   listener-publish  LDT3a18-040Z      1    -
d listener-dispatch    role   listener-publish  LDT3a18-041Z      -    1
d listener-dispatch    role   core-publish      LDT3a18-042Z      1    -
d listener-dispatch    role   core-publish      LDT3a18-043Z      -    1
d listener-dispatch    team   team-publish      LDT3a18-044Z      1    1
e dispatch-kill        issue  comment-1         LDT3a18-045Z      1    1
e dispatch-kill        issue  comment-2         LDT3a18-046Z      1    1
e dispatch-kill        agent  dispatch-message  LDT3a18-047Z      1    -
e dispatch-kill        agent  dispatch-message  LDT3a18-048Z      -    1
e dispatch-kill        agent  listener-send     LDT3a18-049Z      1    -
e dispatch-kill        agent  listener-send     LDT3a18-050Z      -    1
e dispatch-kill        role   listener-publish  LDT3a18-051Z      1    -
e dispatch-kill        role   listener-publish  LDT3a18-052Z      -    1
e dispatch-kill        role   core-publish      LDT3a18-053Z      1    -
e dispatch-kill        role   core-publish      LDT3a18-054Z      -    1
e dispatch-kill        team   team-publish      LDT3a18-055Z      1    1
f none                 issue  comment-1         LDT3a18-056Z      1    1
f none                 issue  comment-2         LDT3a18-057Z      1    1
f none                 agent  dispatch-message  LDT3a18-058Z      1    -
f none                 agent  dispatch-message  LDT3a18-059Z      -    1
f none                 agent  listener-send     LDT3a18-060Z      1    -
f none                 agent  listener-send     LDT3a18-061Z      -    1
f none                 role   listener-publish  LDT3a18-062Z      1    -
f none                 role   listener-publish  LDT3a18-063Z      -    1
f none                 role   core-publish      LDT3a18-064Z      1    -
f none                 role   core-publish      LDT3a18-065Z      -    1
f none                 team   team-publish      LDT3a18-066Z      1    1
(a cell is the count of entries carrying the marker in that session's transcript; "!" marks a count other than expected; "/0" marks an event its publisher refused, expected 0 times; "-" an event not meant for that session)
note: Dispatch published LIVE-1's comments in another order than they were written: written LDT3a18-001Z LDT3a18-002Z LDT3a18-012Z LDT3a18-013Z LDT3a18-023Z LDT3a18-024Z LDT3a18-034Z LDT3a18-035Z LDT3a18-045Z LDT3a18-046Z LDT3a18-056Z LDT3a18-057Z ; stored LDT3a18-001Z LDT3a18-002Z LDT3a18-012Z LDT3a18-013Z LDT3a18-023Z LDT3a18-024Z LDT3a18-034Z LDT3a18-035Z LDT3a18-046Z LDT3a18-045Z LDT3a18-056Z LDT3a18-057Z
issue order compared in pi (12 comments): LDT3a18-001Z@seq4:line17 LDT3a18-002Z@seq5:line18 LDT3a18-012Z@seq19:line40 LDT3a18-013Z@seq20:line44 LDT3a18-023Z@seq34:line63 LDT3a18-024Z@seq35:line67 LDT3a18-034Z@seq49:line86 LDT3a18-035Z@seq50:line90 LDT3a18-046Z@seq71:line126 LDT3a18-045Z@seq76:line130 LDT3a18-056Z@seq77:line134 LDT3a18-057Z@seq78:line138
issue order compared in cc (12 comments): LDT3a18-001Z@seq4:line69 LDT3a18-002Z@seq5:line78 LDT3a18-012Z@seq19:line139 LDT3a18-013Z@seq20:line153 LDT3a18-023Z@seq34:line212 LDT3a18-024Z@seq35:line223 LDT3a18-034Z@seq49:line287 LDT3a18-035Z@seq50:line296 LDT3a18-046Z@seq71:line398 LDT3a18-045Z@seq76:line407 LDT3a18-056Z@seq77:line421 LDT3a18-057Z@seq78:line432
issue order: every comment of LIVE-1 appears in its stream order in both transcripts
transcripts: pi /legion/scratch/tmp/impl-round4/sessions/pi/sessions/2026-11-01T01-43-18-341Z_01a194c6-7c45-7351-81bb-b6f18a64ba20.jsonl; cc /legion/scratch/tmp/impl-round4/sessions/cc/config/projects/-legion-scratch-tmp-impl-round4-sessions-cc-cwd/2ef09cb7-911c-4f01-abf9-13fd612c1158.jsonl
