package heavybodies

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// digestTablePath is the checked-in table the bodies here and the ones e2e/heavy-bodies.ts spells
// are both held to. Every failure below names it, since what a reader has to put right is either
// this package or that file.
const digestTablePath = "testdata/shared-bodies.json"

// sharedRow is one row of the table's shared list: a body both sides spell, with the length in
// bytes and the digest the two are held to.
type sharedRow struct {
	ID               string `json:"id"`
	GoName           string `json:"goName"`
	TypeScriptExport string `json:"typescriptExport"`
	Length           int    `json:"length"` // the body's length in bytes, UTF-8
	SHA256           string `json:"sha256"`
}

// oneSideRow is one row of the table's oneSideOnly list: a body one side has alone, and why.
type oneSideRow struct {
	ID   string `json:"id"`
	Side string `json:"side"`
	Why  string `json:"why"`
}

// digestTable is testdata/shared-bodies.json whole, schema and both lists.
type digestTable struct {
	Schema      string       `json:"schema"`
	Shared      []sharedRow  `json:"shared"`
	OneSideOnly []oneSideRow `json:"oneSideOnly"`
}

// goBody is one body this package spells, under the name its subtests carry.
type goBody struct {
	name string
	body string
}

// goBodies is every body this package spells, by ID: each Shape at the body cap, each nested
// directive whole. Two bodies under one ID would hide one of them from the table, so that fails.
func goBodies(t *testing.T) map[string]goBody {
	t.Helper()
	bodies := make(map[string]goBody)
	add := func(id, name, body string) {
		if id == "" {
			t.Fatalf("%q carries no ID, so %s can pin nothing to it", name, digestTablePath)
		}
		if first, seen := bodies[id]; seen {
			t.Fatalf("%q is the ID of both %q and %q; %s pins one body to an ID", id, first.name, name, digestTablePath)
		}
		bodies[id] = goBody{name: name, body: body}
	}
	for _, shape := range Shapes() {
		add(shape.ID, shape.Name, shape.Grow())
	}
	for _, directive := range NestedDirectives() {
		add(directive.ID, directive.Name, directive.Markdown)
	}
	return bodies
}

// readDigestTable is the table, parsed, or a failure naming the file.
func readDigestTable(t *testing.T) digestTable {
	t.Helper()
	raw, err := os.ReadFile(digestTablePath)
	if err != nil {
		t.Fatalf("read %s: %v", digestTablePath, err)
	}
	var table digestTable
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("parse %s: %v", digestTablePath, err)
	}
	if table.Schema == "" {
		t.Fatalf("%s names no schema, which is where the file says what its rows mean", digestTablePath)
	}
	return table
}

// Every body the table says both sides spell is spelled here as its row says, to the byte and
// to the digest. e2e/__tests__/heavy-bodies-digests.test.ts holds the TypeScript side to the
// same rows, so the two spellings cannot drift apart unseen: a shape respelled here alone fails
// this test, and one respelled there alone fails that one.
func TestSharedBodiesAreTheBytesTheDigestTablePins(t *testing.T) {
	table := readDigestTable(t)
	bodies := goBodies(t)
	if len(table.Shared) == 0 {
		t.Fatalf("%s shares no body with e2e/heavy-bodies.ts, so nothing holds the two sides together", digestTablePath)
	}
	for _, row := range table.Shared {
		t.Run(row.ID, func(t *testing.T) {
			body, ok := bodies[row.ID]
			if !ok {
				t.Fatalf("%s shares %q (the TypeScript %s), which no Shape or NestedDirective here carries as its ID: spell the body again, or move its row to oneSideOnly with why TypeScript alone has it",
					digestTablePath, row.ID, row.TypeScriptExport)
			}
			if body.name != row.GoName {
				t.Errorf("%q is named %q here and %q in %s", row.ID, body.name, row.GoName, digestTablePath)
			}
			sum := sha256.Sum256([]byte(body.body))
			digest := hex.EncodeToString(sum[:])
			if len(body.body) != row.Length || digest != row.SHA256 {
				t.Errorf("%q is %d bytes, sha256 %s, here; %s pins it at %d bytes, sha256 %s. The TypeScript %s is held to the same row, so change both sides or neither",
					row.ID, len(body.body), digest, digestTablePath, row.Length, row.SHA256, row.TypeScriptExport)
			}
		})
	}
}

// Every body this package spells stands in the table exactly once, and every row of it that
// claims Go names a body this package spells. Without this a shape added, dropped or renamed on
// the Go side alone would simply go unmentioned, and the digests above would still pass.
func TestEveryGoBodyStandsOnceInTheDigestTable(t *testing.T) {
	table := readDigestTable(t)
	bodies := goBodies(t)
	// Each ID the table names, against the list it stands in, so a body named twice is caught
	// wherever its second row is.
	listed := make(map[string]string)
	note := func(id, list string) {
		if first, seen := listed[id]; seen {
			t.Errorf("%s names %q twice, in %s and in %s; each body stands in one list once", digestTablePath, id, first, list)
			return
		}
		listed[id] = list
	}
	for _, row := range table.Shared {
		note(row.ID, "shared")
	}
	for _, row := range table.OneSideOnly {
		if row.Side != "go" && row.Side != "typescript" {
			t.Errorf("%s has %q on side %q, which is neither \"go\" nor \"typescript\"", digestTablePath, row.ID, row.Side)
			continue
		}
		if row.Why == "" {
			t.Errorf("%s has %q on one side only and says nowhere why the other side does not spell it", digestTablePath, row.ID)
		}
		note(row.ID, "oneSideOnly ("+row.Side+")")
	}
	for id, body := range bodies {
		switch listed[id] {
		case "shared", "oneSideOnly (go)":
		case "":
			t.Errorf("%q (%q) is spelled here and stands in no list of %s: share it with its length and sha256 where e2e/heavy-bodies.ts spells it too, or list it under oneSideOnly as Go's own, with why",
				id, body.name, digestTablePath)
		default:
			t.Errorf("%q (%q) is spelled here, and %s has it as %s", id, body.name, digestTablePath, listed[id])
		}
	}
	for id, list := range listed {
		if _, ok := bodies[id]; ok || list == "oneSideOnly (typescript)" {
			continue
		}
		t.Errorf("%s's %s row for %q names no body of this package", digestTablePath, list, id)
	}
}
