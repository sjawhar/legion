package sse

import (
	"testing"
)

func TestBroadcastRepoMatchesWatchedSet(t *testing.T) {
	hub := New()
	_, matched := hub.AddClient("sjawhar", []string{"sjawhar/legion", "other/x"})
	_, skipped := hub.AddClient("sjawhar2", []string{"other/x"})
	hub.BroadcastRepo("sjawhar/legion", Event{Type: "github_event", Data: map[string]any{"ok": true}})
	want := "event: github_event\ndata: {\"ok\":true}\n\n"
	if got := string(<-matched.Messages); got != want {
		t.Errorf("matched: got %q want %q", got, want)
	}
	select {
	case msg, ok := <-skipped.Messages:
		if ok {
			t.Errorf("skipped got unexpected: %q", string(msg))
		}
	default:
	}
}

func TestBroadcastRepoLowercases(t *testing.T) {
	hub := New()
	_, c := hub.AddClient("sjawhar", []string{"SJAWHAR/Legion"})
	hub.BroadcastRepo("sjawhar/legion", Event{Type: "x", Data: 1})
	if got := string(<-c.Messages); got != "event: x\ndata: 1\n\n" {
		t.Errorf("got %q", got)
	}
}

func TestEmptyWatchedSetReceivesNothing(t *testing.T) {
	hub := New()
	_, c := hub.AddClient("sjawhar", nil)
	hub.BroadcastRepo("sjawhar/legion", Event{Type: "x", Data: 1})
	select {
	case msg, ok := <-c.Messages:
		if ok {
			t.Errorf("empty-watched got unexpected: %q", string(msg))
		}
	default:
	}
}

func TestBroadcastAllReachesEveryone(t *testing.T) {
	hub := New()
	_, a := hub.AddClient("sjawhar", []string{"x/y"})
	_, b := hub.AddClient("sjawhar2", nil)
	hub.BroadcastAll(Event{Type: "notice", Data: map[string]any{"ok": true}})
	want := "event: notice\ndata: {\"ok\":true}\n\n"
	if got := string(<-a.Messages); got != want {
		t.Errorf("a: got %q", got)
	}
	if got := string(<-b.Messages); got != want {
		t.Errorf("b: got %q", got)
	}
}

func TestRemoveClient(t *testing.T) {
	hub := New()
	id, _ := hub.AddClient("sjawhar", []string{"x/y"})
	hub.RemoveClient(id)
	hub.BroadcastRepo("x/y", Event{Type: "x", Data: nil})
	if hub.Size() != 0 {
		t.Errorf("size: %d", hub.Size())
	}
}

func TestDropsSlowClient(t *testing.T) {
	hub := New()
	_, client := hub.AddClient("sjawhar", []string{"x/y"})
	for i := 0; i < 17; i++ {
		hub.BroadcastRepo("x/y", Event{Type: "x", Data: i})
	}
	dropped := false
	for {
		_, ok := <-client.Messages
		if !ok {
			dropped = true
			break
		}
	}
	if !dropped {
		t.Errorf("expected slow client to be dropped")
	}
}
