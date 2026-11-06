package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The backlog publisher (`loadgen backlog`): it publishes envelopes to the simulated sessions'
// agent subjects (contracts.AgentSubject), as Dispatch's outbox publishes a message meant for one
// session, so that a listener which is down builds up a backlog the sessions must get once it is
// back (restart-step.sh). Every publish goes to the notification stream over JetStream under a
// Nats-Msg-Id of its own, and every acknowledgement is waited for, so the summary counts the
// publishes that failed as well as those the stream stored.

// backlogIDBase is the first event id of a backlog run, far above any marker the event generator
// mints, so a backlog envelope is never taken for a step's write.
const backlogIDBase = 900_000_000

// backlogType is the event type of a backlog envelope's payload.
const backlogType = "loadtest.backlog"

// backlogSummary is what a backlog run writes: its arguments, the id range it published, and how
// the publishes went.
type backlogSummary struct {
	NATS     string  `json:"nats"`
	Sessions int     `json:"sessions"`
	Per      int     `json:"per"`
	OverS    float64 `json:"over_s"`
	IDBase   int64   `json:"id_base"`
	// IDFrom and IDTo bound the event ids this run published, both included.
	IDFrom int64 `json:"id_from"`
	IDTo   int64 `json:"id_to"`
	// Envelopes is how many were offered: Sessions × Per.
	Envelopes int    `json:"envelopes"`
	StartedAt string `json:"started_at"`
	StartedNs int64  `json:"started_ns"`
	EndedNs   int64  `json:"ended_ns"`
	// Published counts the publishes the stream acknowledged, Duplicates those it acknowledged as
	// a repeat of a Nats-Msg-Id it held, PublishErrors those it refused or failed, and Pending
	// those whose acknowledgement had not arrived when the wait ran out. A backlog run is clean
	// when PublishErrors and Pending are 0 and Published is Envelopes.
	Published     int    `json:"published"`
	Duplicates    int    `json:"publish_duplicates"`
	PublishErrors int    `json:"publish_errors"`
	Pending       int    `json:"publish_pending"`
	FirstError    string `json:"first_error,omitempty"`
}

// backlogPlan is one envelope to publish: its session, event id and the time it goes out.
type backlogPlan struct {
	Session string
	ID      int64
	At      time.Duration // after the run's start
}

// planBacklog spreads sessions × per publishes evenly over the given time, in rounds: every
// session gets its first envelope before any gets its second, so a listener that comes back part
// way through has given every session the same share. Event ids run from idBase, in publish order.
func planBacklog(sessions, per int, over time.Duration, idBase int64) []backlogPlan {
	total := sessions * per
	out := make([]backlogPlan, 0, total)
	for i := range total {
		at := time.Duration(0)
		if total > 1 {
			at = time.Duration(int64(over) * int64(i) / int64(total-1))
		}
		out = append(out, backlogPlan{Session: sessionID(i % sessions), ID: idBase + int64(i), At: at})
	}
	return out
}

// backlogEnvelope is the envelope of one planned publish: an agent-sourced message on the
// session's own subject, with the event id in its payload, where relay.go and the plugins read it.
func backlogEnvelope(p backlogPlan) contracts.Envelope {
	id := fmt.Sprintf("lgb-%d", p.ID)
	return contracts.Envelope{
		EventID:        id,
		Source:         "agent",
		SourceEventID:  id,
		SourceSession:  "loadgen-backlog",
		Topic:          contracts.AgentSubject(p.Session),
		DedupeKey:      "agent." + p.Session + "." + id,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "loadtest backlog " + id,
		Payload:        fmt.Sprintf(`{"id":%d,"type":%q}`, p.ID, backlogType),
		TraceID:        id,
	}
}

// backlogMsgID is the Nats-Msg-Id of a planned publish: the stream drops a repeat of it, so a
// backlog run that is started twice by mistake stores one copy of each envelope.
func backlogMsgID(p backlogPlan) string {
	return "loadgen-backlog-" + p.Session + "-" + fmt.Sprint(p.ID)
}

func runBacklog(args []string) error {
	fs := flag.NewFlagSet("backlog", flag.ExitOnError)
	stack := fs.String("stack", "", "stack.json path (default $LOADTEST_ROOT/stack.json)")
	natsURL := fs.String("nats", "", "NATS URL (default: the stack's)")
	sessions := fs.Int("sessions", 0, "number of sessions to publish to (ses-lg-0000 ...)")
	per := fs.Int("per", 1, "envelopes per session")
	over := fs.Duration("over", 25*time.Second, "spread the publishes evenly over this time")
	idBase := fs.Int64("id-base", backlogIDBase, "event id of the first envelope")
	out := fs.String("out", "", "write the summary here as well as to stdout")
	wait := fs.Duration("ack-wait", 60*time.Second, "how long to wait for the acknowledgements once every envelope is sent")
	_ = fs.Parse(args)
	if *sessions <= 0 || *per <= 0 {
		return fmt.Errorf("-sessions and -per are required and must be above 0")
	}
	if *over < 0 {
		return fmt.Errorf("-over may not be negative")
	}
	if *idBase <= 0 {
		return fmt.Errorf("-id-base must be above 0")
	}
	url, monURL, name, err := backlogTarget(*stack, *natsURL)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := checkNATSServerName(ctx, &http.Client{Timeout: 10 * time.Second}, monURL, name); err != nil {
		return err
	}
	sum, err := publishBacklog(url, planBacklog(*sessions, *per, *over, *idBase), *wait)
	if err != nil {
		return err
	}
	sum.NATS, sum.Sessions, sum.Per, sum.OverS, sum.IDBase = redactEndpoint("-nats", url), *sessions, *per, over.Seconds(), *idBase
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("%s\n", b)
	if sum.PublishErrors > 0 || sum.Pending > 0 {
		// A publish the stream did not acknowledge is counted, not fatal: the run goes on, and the
		// restart report shows the count beside the envelopes the sessions received. A run that
		// published nothing reached no stream at all, and that is an error.
		fmt.Fprintf(os.Stderr, "loadgen backlog: %d of %d publishes failed and %d were not acknowledged: %s\n",
			sum.PublishErrors, sum.Envelopes, sum.Pending, sum.FirstError)
	}
	if sum.Published == 0 && sum.Envelopes > 0 {
		return fmt.Errorf("none of the %d envelopes was stored: %s", sum.Envelopes, sum.FirstError)
	}
	return nil
}

// backlogTarget is the NATS URL to publish to, the monitoring URL that names the server, and the
// name the stack expects there. A -nats that differs from the stack's is refused, as the endpoint
// flags of `loadgen run` are: a backlog run writes to sessions' own subjects, and the pod's live
// NATS must never be one of them.
func backlogTarget(stackPath, given string) (url, monURL, name string, err error) {
	if stackPath == "" {
		stackPath = filepath.Join(loadtestRoot(), "stack.json")
	}
	st, err := loadStack(stackPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", "", fmt.Errorf("read stack: %w (stack.sh setup writes it)", err)
		}
		return "", "", "", fmt.Errorf("read stack: %w", err)
	}
	url = fmt.Sprintf("nats://127.0.0.1:%d", st.Ports.NATS)
	if given != "" && given != url {
		return "", "", "", fmt.Errorf("-nats %s is not the NATS of the stack in %s (%s); a backlog run publishes to the sessions' own subjects, so it is refused",
			redactEndpoint("-nats", given), stackPath, url)
	}
	return url, fmt.Sprintf("http://127.0.0.1:%d", st.Ports.NATSMonitor), st.Name, nil
}

// publishBacklog publishes every planned envelope on its own schedule and waits up to wait for the
// stream's acknowledgements once the last one is sent. A publish whose acknowledgement never
// arrives is counted (Pending), never taken for a stored message.
func publishBacklog(url string, plans []backlogPlan, wait time.Duration) (backlogSummary, error) {
	nc, err := nats.Connect(url, nats.Name("loadgen-backlog"), nats.MaxReconnects(-1))
	if err != nil {
		return backlogSummary{}, fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.PublishAsyncMaxPending(1024))
	if err != nil {
		return backlogSummary{}, fmt.Errorf("jetstream: %w", err)
	}
	sum := backlogSummary{Envelopes: len(plans)}
	if len(plans) > 0 {
		sum.IDFrom, sum.IDTo = plans[0].ID, plans[len(plans)-1].ID
	}
	start := time.Now()
	sum.StartedAt, sum.StartedNs = start.UTC().Format(time.RFC3339Nano), start.UnixNano()
	futures := make([]nats.PubAckFuture, 0, len(plans))
	for _, p := range plans {
		if d := time.Until(start.Add(p.At)); d > 0 {
			time.Sleep(d)
		}
		data, err := json.Marshal(backlogEnvelope(p))
		if err != nil {
			return sum, err
		}
		future, err := js.PublishMsgAsync(&nats.Msg{Subject: contracts.AgentSubject(p.Session), Data: data}, nats.MsgId(backlogMsgID(p)))
		if err != nil {
			sum.PublishErrors++
			if sum.FirstError == "" {
				sum.FirstError = err.Error()
			}
			continue
		}
		futures = append(futures, future)
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(wait):
	}
	for _, f := range futures {
		select {
		case ack := <-f.Ok():
			sum.Published++
			if ack.Duplicate {
				sum.Duplicates++
			}
		case err := <-f.Err():
			sum.PublishErrors++
			if sum.FirstError == "" {
				sum.FirstError = err.Error()
			}
		default:
			sum.Pending++
			if sum.FirstError == "" {
				sum.FirstError = "no acknowledgement within " + wait.String()
			}
		}
	}
	sum.EndedNs = time.Now().UnixNano()
	return sum, nil
}
