package rules

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestCurrentKeepsPreviousRulesWhenReloadIsInvalid(t *testing.T) {
	valid, _ := os.ReadFile("testdata/valid.yaml")
	path := t.TempDir() + "/rules.yaml"
	os.WriteFile(path, valid, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	alarmed := make(chan error, 1)
	cur, err := NewCurrent(ctx, FileLoader{Path: path}, 10*time.Millisecond, func(e error) {
		select {
		case alarmed <- e:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	first := cur.Get().Version
	os.WriteFile(path, []byte("version: 2\n"), 0o600)
	select {
	case <-alarmed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected an alarm")
	}
	if cur.Get().Version != first {
		t.Fatal("an invalid reload must not replace the live rules")
	}
}

func TestNewCurrentRefusesInvalidFirstLoad(t *testing.T) {
	path := t.TempDir() + "/rules.yaml"
	os.WriteFile(path, []byte("version: 2\n"), 0o600)
	if _, err := NewCurrent(context.Background(), FileLoader{Path: path}, time.Hour, func(error) {}); err == nil {
		t.Fatal("first load must refuse an invalid file")
	}
}

// TestNewCurrentOnReloadHook pins the optional onReload hook Task 8 wires to
// approvers.Service.Reconcile: it sees every successfully parsed Set before it is adopted, its
// refusal fails the first load exactly like an invalid file, and its refusal on a later reload
// keeps the previous set and alarms exactly like an invalid file — never wired at all (the
// pre-existing tests above) behaves identically to always accepting.
func TestNewCurrentOnReloadHook(t *testing.T) {
	valid, _ := os.ReadFile("testdata/valid.yaml")
	path := t.TempDir() + "/rules.yaml"
	os.WriteFile(path, valid, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refusal := errors.New("reconcile refused")
	if _, err := NewCurrent(ctx, FileLoader{Path: path}, time.Hour, func(error) {}, func(*Set) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("a hook refusing the first load must fail NewCurrent: %v", err)
	}

	calls := 0
	alarmed := make(chan error, 1)
	cur, err := NewCurrent(ctx, FileLoader{Path: path}, 10*time.Millisecond, func(e error) {
		select {
		case alarmed <- e:
		default:
		}
	}, func(s *Set) error {
		calls++
		if calls == 1 {
			return nil // adopt the first load
		}
		return refusal // refuse every reload after
	})
	if err != nil {
		t.Fatal(err)
	}
	first := cur.Get()
	// The ticker reloads the file on every tick, so the next tick is the reload the hook refuses.
	// Rewriting the file here would let that reload read it empty, between truncate and write.
	select {
	case e := <-alarmed:
		if !errors.Is(e, refusal) {
			t.Fatalf("alarm error = %v, want it to wrap %v", e, refusal)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected the reload hook's refusal to alarm")
	}
	if cur.Get() != first {
		t.Fatal("a reload the hook refuses must not replace the live rules")
	}
}
