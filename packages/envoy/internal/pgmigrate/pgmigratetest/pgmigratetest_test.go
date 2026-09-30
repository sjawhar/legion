package pgmigratetest

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestCheckNumberedOneToNRefusesAGap(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_a.up.sql": {Data: []byte("select 1")},
		"migrations/0002_b.up.sql": {Data: []byte("select 2")},
		"migrations/0004_d.up.sql": {Data: []byte("select 4")},
	}
	err := CheckNumberedOneToN(set, "migrations")
	if err == nil || !strings.Contains(err.Error(), "migration 0004_d.up.sql is numbered 4 where 3 comes next") {
		t.Fatalf("CheckNumberedOneToN = %v, want it to name 0004_d.up.sql and the missing 3", err)
	}
}

func TestCheckNumberedOneToNRefusesASetNotStartingAtOne(t *testing.T) {
	set := fstest.MapFS{"migrations/0002_b.up.sql": {Data: []byte("select 2")}}
	err := CheckNumberedOneToN(set, "migrations")
	if err == nil || !strings.Contains(err.Error(), "numbered 2 where 1 comes next") {
		t.Fatalf("CheckNumberedOneToN = %v, want it to name the missing 1", err)
	}
}

func TestCheckNumberedOneToNAcceptsOneToN(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_a.up.sql":   {Data: []byte("select 1")},
		"migrations/0001_a.down.sql": {Data: []byte("select -1")},
		"migrations/2_b.up.sql":      {Data: []byte("select 2")},
	}
	if err := CheckNumberedOneToN(set, "migrations"); err != nil {
		t.Fatalf("CheckNumberedOneToN = %v, want nil", err)
	}
}
