import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

const out = resolve(
  import.meta.dir,
  "../../envoy/internal/contracts/generated.go",
);

mkdirSync(dirname(out), { recursive: true });

writeFileSync(
  out,
  `package contracts

import (
\t"fmt"
\t"strings"
\t"time"
)

type Envelope struct {
\tEventID        string \
\t\t\t\t\t\t\t\t\`json:"event_id"\`
\tSource         string \
\t\t\t\t\t\t\t\t\`json:"source"\`
\tSourceEventID  string \
\t\t\t\t\t\t\t\t\`json:"source_event_id"\`
\tTopic          string \
\t\t\t\t\t\t\t\t\`json:"topic"\`
\tDedupeKey      string \
\t\t\t\t\t\t\t\t\`json:"dedupe_key"\`
\tIssuedAt       int64  \
\t\t\t\t\t\t\t\t\`json:"issued_at"\`
\tExpiresAt      *int64 \
\t\t\t\t\t\t\t\t\`json:"expires_at,omitempty"\`
\tPayloadSummary string \
\t\t\t\t\t\t\t\t\`json:"payload_summary"\`
\tPayloadRef     string \
\t\t\t\t\t\t\t\t\`json:"payload_ref,omitempty"\`
\tTraceID        string \
\t\t\t\t\t\t\t\t\`json:"trace_id"\`
}

func (e Envelope) Validate() error {
\tif strings.TrimSpace(e.EventID) == "" {
\t\treturn fmt.Errorf("event_id is required")
\t}
\tif strings.TrimSpace(e.Source) == "" {
\t\treturn fmt.Errorf("source is required")
\t}
\tif strings.TrimSpace(e.SourceEventID) == "" {
\t\treturn fmt.Errorf("source_event_id is required")
\t}
\tif strings.TrimSpace(e.Topic) == "" {
\t\treturn fmt.Errorf("topic is required")
\t}
\tif strings.TrimSpace(e.DedupeKey) == "" {
\t\treturn fmt.Errorf("dedupe_key is required")
\t}
\tif strings.TrimSpace(e.PayloadSummary) == "" {
\t\treturn fmt.Errorf("payload_summary is required")
\t}
\tif strings.TrimSpace(e.TraceID) == "" {
\t\treturn fmt.Errorf("trace_id is required")
\t}
\tif e.IssuedAt <= 0 {
\t\treturn fmt.Errorf("issued_at must be set")
\t}
\treturn nil
}

func NowMillis() int64 {
\treturn time.Now().UnixMilli()
}

func AgentSubject(session string) string {
\treturn "notifications.agent." + session
}

func GithubSubject(owner string, repo string, kind string) string {
\treturn "notifications.github." + owner + "." + repo + "." + kind
}

func SlackSubject(team string, channel string, kind string) string {
\treturn "notifications.slack." + team + "." + channel + "." + kind
}
`,
);
