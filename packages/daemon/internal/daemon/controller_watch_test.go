package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// syncBuffer is a log sink a daemon writes while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The daemon launches no controller under either runtime, so it says when none is registered, or
// when the Envoy role registry says the registered one is gone, and how to start one — once per
// worker boot timeout, and never about a controller the registry holds alive.
func TestTheDaemonSaysWhenNoControllerIsRegistered(t *testing.T) {
	const notRegistered = "controller not registered; run legion controller start"
	for _, tc := range []struct {
		name     string
		register bool
		holder   func(session string) (int, any)
		want     int
	}{
		{"no controller has registered", false, nil, 1},
		{"the registered session holds the role", true, func(session string) (int, any) {
			return http.StatusOK, map[string]any{"holder": session, "last_seen": time.Now().UnixMilli()}
		}, 0},
		{"the role has no live holder", true, func(string) (int, any) { return http.StatusNotFound, map[string]any{} }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			const session = "ses_controller"
			var lookups atomic.Int64
			envoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				code, body := http.StatusNotFound, any(map[string]any{})
				if tc.holder != nil {
					code, body = tc.holder(session)
				}
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(body)
				lookups.Add(1)
			}))
			defer envoy.Close()
			cfg.EnvoyURL = envoy.URL
			project, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			if tc.register {
				st, err := store.Open(context.Background(), cfg.PostgresDSN)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.Migrate(context.Background()); err != nil {
					t.Fatal(err)
				}
				generation, err := st.MintController(context.Background(), project, []byte("capability"))
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := st.RegisterController(context.Background(), project, generation, session, []byte("secret"), time.Now()); err != nil || !ok {
					t.Fatalf("register the controller: %v %v", ok, err)
				}
				st.Close()
			}
			logs := &syncBuffer{}
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.orphanSweep = 20 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)), o) }()
			awaitHealthz(t, cfg, done)
			// The first sweep runs as the daemon starts serving, and a due line comes from it. The
			// count then waits out further sweeps, where a second line would show: a registered
			// controller is looked up in the role registry on every sweep, so three more lookups are
			// three more sweeps; with none registered nothing is looked up, and ten intervals pass.
			if tc.want > 0 {
				testwait.Eventually(t, "the not-registered line", func() bool { return strings.Contains(logs.String(), notRegistered) })
			}
			if tc.register {
				seen := lookups.Load()
				testwait.Eventually(t, "three more sweeps", func() bool { return lookups.Load() >= seen+3 })
			} else {
				time.Sleep(10 * o.orphanSweep)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := strings.Count(logs.String(), notRegistered); got != tc.want {
				t.Errorf("%q was logged %d times over many sweeps, want %d\n%s", notRegistered, got, tc.want, logs.String())
			}
		})
	}
}
