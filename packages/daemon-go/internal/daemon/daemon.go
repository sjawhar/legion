// Package daemon runs one legion: it opens the store its configuration names, brings the schema
// forward, records the boot, serves the API, and stops in the order the durable record needs.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"golang.org/x/sync/errgroup"
)

const (
	// bootTimeout bounds the work between the process starting and the API listening: an
	// unreachable Postgres refuses in milliseconds, but a reachable one that never answers must
	// not leave the daemon hanging with nothing on stderr.
	bootTimeout = 30 * time.Second
	// shutdownTimeout bounds each half of the exit — draining the API, then stamping the boot.
	shutdownTimeout = 10 * time.Second
)

// Run is the daemon. It opens the store (refusing by the host it could not reach), migrates,
// takes its listener, records the boot, serves the API, and blocks until ctx is done — then
// closes the API, stamps the boot's end, and closes the pool, in that order, because the stamp
// needs the pool.
//
// The listener comes before the boot record: a recorded boot is a boot that served, and a daemon
// whose port another process holds never ran.
//
// ctx decides one thing: how long the daemon serves. The boot record and its stamp are the
// daemon's own bookkeeping and run on a context the shutdown did not cancel, so a signal that
// arrives mid-startup still leaves a recorded, stamped boot rather than a row with no end.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	boot, cancelBoot := context.WithTimeout(context.WithoutCancel(ctx), bootTimeout)
	defer cancelBoot()

	st, err := store.Open(boot, cfg.PostgresDSN)
	if err != nil {
		return err
	}

	applied, err := st.Migrate(boot)
	if err != nil {
		st.Close()
		return err
	}

	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		st.Close()
		return fmt.Errorf("listen on %s: %w", address, err)
	}

	startedAt := time.Now().UTC()
	bootID, err := st.RecordBoot(boot, cfg.Project, startedAt)
	if err != nil {
		listener.Close()
		st.Close()
		return err
	}
	log.Info("legion daemon started",
		"project", cfg.Project,
		"address", address,
		"runtime", cfg.Runtime.Name,
		"admissionCap", cfg.AdmissionCap,
		"migrationsApplied", applied,
		"boot", bootID,
	)

	serveErr := serve(ctx, cfg, st, startedAt, listener)

	stop, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancelStop()
	stopErr := st.StopBoot(stop, bootID, time.Now().UTC())
	st.Close()
	log.Info("legion daemon stopped", "project", cfg.Project, "boot", bootID)

	return errors.Join(serveErr, stopErr)
}

// serve runs the API on the listener the daemon already took until ctx is done or the server
// fails, and returns once it is closed: one goroutine serves, the other shuts down, and the
// shutdown runs on a context of its own so a cancelled ctx still drains the connections it has.
func serve(ctx context.Context, cfg config.Config, st *store.Store, startedAt time.Time, listener net.Listener) error {
	server := api.NewServer(cfg.Bind, cfg.Port, &source{
		store:        st,
		project:      cfg.Project,
		admissionCap: cfg.AdmissionCap,
		startedAt:    startedAt,
	})

	group, serving := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve the API on %s: %w", server.Addr, err)
		}
		return nil
	})
	group.Go(func() error {
		<-serving.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		return server.Shutdown(shutdown)
	})
	return group.Wait()
}

// source answers the state route out of the daemon's own store. Stage 1 has no issues to report:
// the record is the daemon itself and the cap it admits under.
type source struct {
	store        *store.Store
	project      string
	admissionCap int
	startedAt    time.Time
}

func (s *source) State(ctx context.Context) (api.State, error) {
	version, err := s.store.SchemaVersion(ctx)
	if err != nil {
		return api.State{}, err
	}
	boots, firstBootAt, err := s.store.Boots(ctx, s.project)
	if err != nil {
		return api.State{}, err
	}
	return api.State{
		Daemon: api.DaemonInfo{
			Project:       s.project,
			SchemaVersion: version,
			Boots:         boots,
			FirstBootAt:   firstBootAt,
			StartedAt:     s.startedAt,
		},
		Admission: api.Admission{Cap: s.admissionCap},
		Issues:    map[string]api.Issue{},
	}, nil
}
