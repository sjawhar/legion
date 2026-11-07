package daemon

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/settings"
)

// reportedSettings is the configuration as the state reports it (`settings`), behind the pointer
// every read of the state goes through. It is built once at boot and again at every applied
// change, by either arm: the state must report the value the daemon is running on, beside where
// that value came from (State.SettingSources), and a reader of the Legion view's settings
// control reads the two together.
//
// The rebuild is an observer's, not a request's: a state read costs one atomic load, and the
// work of rendering every setting is done once per change rather than once per reader.
type reportedSettings struct {
	current atomic.Pointer[api.Settings]
}

func newReportedSettings(cfg config.Config) *reportedSettings {
	reported := &reportedSettings{}
	settled := api.SettingsOf(cfg)
	reported.current.Store(&settled)
	return reported
}

// observe is what config.Live calls once an apply has settled. It runs under the holder's lock
// and reads only the configuration it is handed.
func (r *reportedSettings) observe(cfg *config.Config, applied []config.Change) {
	if len(applied) == 0 {
		return
	}
	settled := api.SettingsOf(*cfg)
	r.current.Store(&settled)
}

// get is the settings this read of the state reports.
func (r *reportedSettings) get() api.Settings { return *r.current.Load() }

// restoredSources is the provenance of the route changes this boot laid back over the file: each
// path the route's, with the person who asked for it and the moment the daemon applied it, which
// a restart keeps (api.SettingProvenance.Restore).
func restoredSources(restored []settings.Change) map[string]api.SettingSource {
	if len(restored) == 0 {
		return nil
	}
	sources := make(map[string]api.SettingSource, len(restored))
	for _, change := range restored {
		sources[change.Path] = api.SettingSource{Source: change.Source, By: change.By, At: change.At.UTC()}
	}
	return sources
}

// announcedSettingsQueue is how many announcements wait for the publisher before one is dropped.
// An operator's settings change is rare and the publish takes a moment, so the queue is deep
// enough that no plausible burst of them is lost and shallow enough to cost nothing.
const announcedSettingsQueue = 64

// settingsAnnouncer is the daemon's record of every settings change it applies, by either arm:
// one log line per change naming the key, the value before and after, where the change came from
// and who asked for it (contract 5), and one post to the project's controller topic, so the
// controller — which owns the admission cap — sees every change in its capacity checks.
//
// The log line is written where the change settles, under the settings holder's lock, so the log
// records the changes in the order the daemon applied them. The post is not: publishing is an
// HTTP call to the Envoy listener, and a listener that is slow or down must not hold the lock
// every reader of the configuration takes. The announcements queue instead, and one goroutine
// publishes them in order.
type settingsAnnouncer struct {
	topic     string
	publisher notify.Publisher
	log       *slog.Logger
	queued    chan announcedSetting
	now       func() time.Time
}

// announcedSetting is one applied change on its way to the controller.
type announcedSetting struct {
	change config.Change
	at     time.Time
}

// newSettingsAnnouncer announces project's changes through publisher; a nil publisher posts
// nothing and still logs every change.
func newSettingsAnnouncer(project string, publisher notify.Publisher, log *slog.Logger) *settingsAnnouncer {
	return &settingsAnnouncer{
		topic:     notify.ControllerTopic(project),
		publisher: publisher,
		log:       log,
		queued:    make(chan announcedSetting, announcedSettingsQueue),
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// observe is what config.Live calls once an apply has settled: one log line per change, and each
// change queued for the controller.
func (a *settingsAnnouncer) observe(_ *config.Config, applied []config.Change) {
	at := a.now()
	for _, change := range applied {
		a.log.Info("legion settings: a setting changed",
			"path", change.Path, "old", change.Old, "new", change.New,
			"source", change.Source, "by", change.By,
			"class", string(change.Class), "outcome", string(change.Outcome), "holder", change.Holder)
		if a.publisher == nil {
			continue
		}
		select {
		case a.queued <- announcedSetting{change: change, at: at}:
		default:
			a.log.Warn("legion settings: the controller's notice of a settings change was dropped; its queue is full",
				"path", change.Path, "topic", a.topic)
		}
	}
}

// run publishes the queued announcements until ctx ends. A publish that fails is logged and not
// retried: the change is applied either way, the log line is its record, and the controller
// reads the daemon's settings and their sources in the state at its next tick.
func (a *settingsAnnouncer) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case announced := <-a.queued:
			a.publish(ctx, announced)
		}
	}
}

func (a *settingsAnnouncer) publish(ctx context.Context, announced announcedSetting) {
	change := announced.change
	message := "legion settings: " + change.Path + " " + change.Old + ", now " + change.New
	payload := map[string]any{
		"kind":    "settings.changed",
		"path":    change.Path,
		"old":     change.Old,
		"new":     change.New,
		"class":   string(change.Class),
		"outcome": string(change.Outcome),
		"holder":  change.Holder,
		"source":  change.Source,
		"by":      change.By,
		"at":      announced.at.Format(time.RFC3339Nano),
	}
	dedupe := "legion-settings:" + change.Path + ":" + announced.at.Format(time.RFC3339Nano)
	if err := a.publisher.Publish(ctx, a.topic, message, payload, dedupe); err != nil && ctx.Err() == nil {
		a.log.Warn("legion settings: the controller was not told of a settings change",
			"path", change.Path, "topic", a.topic, "error", err)
	}
}
