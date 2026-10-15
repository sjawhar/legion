package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// JobIntake records the jobs of a deploy run Dispatch already tracks as each job finishes, from the
// job events Envoy publishes for a run with no pull request, so a deploy shows once its production
// job finishes rather than when its run ends (LEGION-690). It reads a run's jobs from GitHub, never
// from the event, once per burst: a run's events within jobQuiet of each other are one read, and a
// burst never waits longer than jobMaxWait. A run it does not track costs no GitHub call.
//
// It has a durable consumer of its own rather than a kind in Intake's route: Intake handles one
// message at a time with its GitHub calls inline (intakeMaxAckPending), and a deploy run's thirty
// jobs finishing together would queue every merge behind them. Intake's own durable filters every
// GitHub subject, so it is handed job messages too, and its route acks them with no call.
type JobIntake struct {
	nats   *bus.Client
	pool   *store.Pool
	github *githubapp.Client
	clock  jobClock

	// ctx is Run's context: every read a timer starts runs under it, so a stopped process stops them.
	ctx context.Context

	// reads holds a slot for each read in flight. When many runs' bursts end at once, as on the
	// first start after an outage drains its backlog, at most reconcilePartialConcurrency runs'
	// jobs are read from GitHub at a time, the bound the reconcile completes pull requests under;
	// one listing per run at once would pass GitHub's 100 concurrent requests and rate-limit the
	// App.
	reads chan struct{}

	mu      sync.Mutex
	pending map[runKey]*pendingRun
}

// runKey names one run of one repository.
type runKey struct {
	repo  string
	runID int64
}

// pendingRun is one run's burst: when its first event arrived, and the timer that reads the run's
// jobs once the burst goes quiet or has waited jobMaxWait.
type pendingRun struct {
	first time.Time
	timer jobTimer
}

const (
	jobConsumerName  = "delivery-jobs"
	jobSubject       = "notifications.github.*.*.job.>"
	jobQuiet         = 2 * time.Second
	jobMaxWait       = 10 * time.Second
	jobMaxAckPending = 64
	// jobAckWait only bounds a message the handler never reached: every message is acked on
	// receipt, before its checks.
	jobAckWait = 30 * time.Second
	// jobCheckTimeout bounds one message's two indexed reads (the settings and RunTracked).
	jobCheckTimeout = 10 * time.Second
)

// jobClock is the time JobIntake schedules by; a test hands it a clock it moves itself.
type jobClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) jobTimer
}

type jobTimer interface{ Stop() bool }

type realJobClock struct{}

func (realJobClock) Now() time.Time { return time.Now() }

func (realJobClock) AfterFunc(d time.Duration, f func()) jobTimer { return time.AfterFunc(d, f) }

// newJobIntake builds a JobIntake on natsClient, a connected client StartIntakes opens for it alone,
// scheduling its reads by clock.
func newJobIntake(natsClient *bus.Client, pool *store.Pool, github *githubapp.Client, clock jobClock) *JobIntake {
	return &JobIntake{
		nats: natsClient, pool: pool, github: github, clock: clock,
		ctx: context.Background(), pending: map[runKey]*pendingRun{},
		reads: make(chan struct{}, reconcilePartialConcurrency),
	}
}

// Run binds the durable job consumer and handles job events until ctx is cancelled. A bind that
// fails is tried again every settingsPollInterval.
func (j *JobIntake) Run(ctx context.Context) {
	j.ctx = ctx
	defer j.stopPending()
	var sub *natsgo.Subscription
	for sub == nil {
		var ok bool
		if sub, ok = j.bind(); ok {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(settingsPollInterval):
		}
	}
	defer func() { _ = sub.Unsubscribe() }()
	<-ctx.Done()
}

// jobConsumerConfig is the durable's policy: deliveryConsumerConfig's shape with this consumer's
// name and filter, starting at new messages, since a job event older than the process says nothing
// the run's end event and the reconcile do not.
func jobConsumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        jobConsumerName,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  jobSubject,
		DeliverPolicy:  natsgo.DeliverNewPolicy,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        jobAckWait,
		MaxAckPending:  jobMaxAckPending,
	}
}

// bind creates (on first run) or resumes the durable and subscribes to it.
func (j *JobIntake) bind() (*natsgo.Subscription, bool) {
	return bindDurableConsumer(j.nats, jobConsumerConfig(), j.deliver)
}

// deliver acks a job message on receipt, then hands its payload to receive. Acking first is safe:
// the run's end event (Intake) and the reconcile both read every job of a run, so a process that
// dies with a read pending loses only the early signal, and a message never waits on GitHub.
func (j *JobIntake) deliver(msg *natsgo.Msg) {
	if err := msg.Ack(); err != nil {
		slog.Error("dispatch delivery: acknowledge job message", "subject", msg.Subject, "error", err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		slog.Error("dispatch delivery: decode job envelope", "subject", msg.Subject, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(j.ctx, jobCheckTimeout)
	defer cancel()
	j.receive(ctx, envelope.Payload)
}

// receive schedules a read of the run a job event names, when it is a run of the deploy repository
// Dispatch already tracks; any other event costs at most these two indexed reads.
func (j *JobIntake) receive(ctx context.Context, payload string) {
	var job struct {
		Kind  string `json:"kind"`
		Repo  string `json:"repo"`
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(payload), &job); err != nil || job.Kind != "job" {
		return
	}
	settings, err := GetSettings(ctx, j.pool)
	if err != nil {
		if !errors.Is(err, ErrNoSettings) {
			slog.Error("dispatch delivery: read settings for a job event", "error", err)
		}
		return
	}
	if job.Repo != settings.DeployRepo {
		return
	}
	runID, err := strconv.ParseInt(job.RunID, 10, 64)
	if err != nil {
		slog.Warn("dispatch delivery: job event with a malformed run_id", "run_id", job.RunID)
		return
	}
	tracked, err := RunTracked(ctx, j.pool, job.Repo, runID)
	if err != nil {
		slog.Error("dispatch delivery: read whether a job's run is tracked", "repo", job.Repo, "run_id", runID, "error", err)
		return
	}
	if !tracked {
		return
	}
	j.schedule(runKey{repo: job.Repo, runID: runID})
}

// schedule starts a run's burst or moves its read to jobQuiet after this event, but never past
// jobMaxWait after the burst's first event.
func (j *JobIntake) schedule(key runKey) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.clock.Now()
	run, ok := j.pending[key]
	if !ok {
		run = &pendingRun{first: now}
		j.pending[key] = run
	} else {
		run.timer.Stop()
	}
	due := now.Add(jobQuiet)
	if limit := run.first.Add(jobMaxWait); limit.Before(due) {
		due = limit
	}
	run.timer = j.clock.AfterFunc(due.Sub(now), func() { j.fire(key, run) })
}

// fire ends a run's burst and reads its jobs once a read slot is free (reads). An event that
// arrives during the read starts the next burst; two reads of one run that overlap are safe, since
// a read never undoes a later one (UpsertRunJobs). The read's deadline starts once it has its slot.
func (j *JobIntake) fire(key runKey, run *pendingRun) {
	j.mu.Lock()
	if j.pending[key] != run {
		j.mu.Unlock()
		return
	}
	delete(j.pending, key)
	j.mu.Unlock()
	select {
	case j.reads <- struct{}{}:
	case <-j.ctx.Done():
		return
	}
	defer func() { <-j.reads }()
	ctx, cancel := context.WithTimeout(j.ctx, intakeMessageTimeout)
	defer cancel()
	j.read(ctx, key)
}

// read lists the run's jobs from GitHub and stores that listing. It marks the run's jobs complete
// only when the same listing shows every job finished, so a listing taken before the run ended
// never marks the run complete because another consumer recorded its end meanwhile.
func (j *JobIntake) read(ctx context.Context, key runKey) {
	owner, repo, err := splitRepo(key.repo)
	if err != nil {
		slog.Error("dispatch delivery: job run repository", "repo", key.repo, "error", err)
		return
	}
	fetched, err := ListWorkflowRunJobs(ctx, j.github, owner, repo, key.runID)
	if err != nil {
		slog.Warn("dispatch delivery: read the jobs of a running deploy run; its end reads them again", "repo", key.repo, "run_id", key.runID, "error", err)
		return
	}
	jobs := RunJobs(key.repo, key.runID, fetched)
	if err := UpsertRunJobs(ctx, j.pool, key.repo, key.runID, jobs, JobsComplete(jobs)); err != nil {
		slog.Error("dispatch delivery: store the jobs of a running deploy run", "repo", key.repo, "run_id", key.runID, "error", err)
	}
}

// stopPending stops every burst's timer once Run ends.
func (j *JobIntake) stopPending() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for key, run := range j.pending {
		run.timer.Stop()
		delete(j.pending, key)
	}
}
