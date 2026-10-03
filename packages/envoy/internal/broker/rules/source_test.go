package rules

import (
	"context"
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
