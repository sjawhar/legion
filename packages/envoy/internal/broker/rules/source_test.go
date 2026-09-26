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
	var alarms []error
	cur, err := NewCurrent(context.Background(), FileLoader{Path: path}, 10*time.Millisecond, func(e error) { alarms = append(alarms, e) })
	if err != nil {
		t.Fatal(err)
	}
	first := cur.Get().Version
	os.WriteFile(path, []byte("version: 2\n"), 0o600)
	time.Sleep(50 * time.Millisecond)
	if cur.Get().Version != first {
		t.Fatal("an invalid reload must not replace the live rules")
	}
	if len(alarms) == 0 {
		t.Fatal("an invalid reload must alarm")
	}
}

func TestNewCurrentRefusesInvalidFirstLoad(t *testing.T) {
	path := t.TempDir() + "/rules.yaml"
	os.WriteFile(path, []byte("version: 2\n"), 0o600)
	if _, err := NewCurrent(context.Background(), FileLoader{Path: path}, time.Hour, func(error) {}); err == nil {
		t.Fatal("first load must refuse an invalid file")
	}
}
