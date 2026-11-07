package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The section of docs/deployment.md this test reads, and the bound of it. The heading opens the
// section and the next `## ` heading closes it, so a table or a reason written elsewhere in the
// document answers for nothing here.
const (
	deploymentDocHeading    = "## What a change of each `legion.yaml` key needs"
	deploymentDocRefusals   = "### Why a fixed key is refused"
	deploymentDocHeadingTop = "## "
)

// docRow is one row of that section's table: the class the document gives a key, and the holder
// it names ("" for the em dash, which says nothing holds the value).
type docRow struct {
	class  string
	holder string
	line   int
}

// The settings table of docs/deployment.md is what an operator reads before changing a key under
// a running daemon: whether the change is applied at once, which holder is built again for it,
// whether it waits for the next launch, or whether the route refuses it outright. A key missing
// from it is the failure this guards — the operator reads the table, does not find the key, and
// either restarts a daemon that needed no restart or changes a key the route will refuse.
//
// The document is held to the code and not the other way round: Classes is the daemon's own
// table, the route and the watcher answer from it, and this test fails naming every key the two
// disagree about. It reads the class and the holder of each row, since a holder is what the
// daemon's log line and the route's answer name, and each fixed key's reason, since that reason
// is the sentence the route refuses with.
func TestDeploymentDocCoversEverySettingsKey(t *testing.T) {
	doc, firstLine := readDeploymentDoc(t)
	rows := deploymentDocTable(t, doc, firstLine)

	for _, path := range Paths() {
		row, documented := rows[path]
		if !documented {
			t.Errorf("docs/deployment.md's settings table has no row for %s: every key of the class table "+
				"belongs in it, with its class and its holder", path)
			continue
		}
		want, _ := ClassOf(path)
		if row.class != string(want.Class) {
			t.Errorf("docs/deployment.md:%d gives %s the class %q; the code says %q",
				row.line, path, row.class, want.Class)
		}
		if row.holder != want.Holder {
			t.Errorf("docs/deployment.md:%d names %s's holder %q; the code says %q (an em dash where the code "+
				"names none)", row.line, path, row.holder, want.Holder)
		}
	}

	known := Paths()
	for path, row := range rows {
		if !slices.Contains(known, path) {
			t.Errorf("docs/deployment.md:%d names %s, which the class table does not hold: a key that is no "+
				"legion.yaml key tells an operator to write one the loader refuses", row.line, path)
		}
	}

	// The refusals are quoted rather than paraphrased, so an operator reading the document reads
	// what the route will say. Backticks are dropped and whitespace folded: the document marks
	// LEGION_LISTEN_FD as code and breaks a long sentence across lines.
	flat := flattenDeploymentDoc(doc)
	for _, row := range Classes {
		if row.Class != ClassFixed {
			continue
		}
		if !strings.Contains(flat, flattenDeploymentText(row.Why)) {
			t.Errorf("docs/deployment.md does not carry the reason %s is refused with, as the route answers "+
				"it: %q", row.Path, row.Why)
		}
	}
}

// readDeploymentDoc is the section's own lines, the heading first, and the line of the file the
// heading stands on. The document is at the repository root, four directories above this package;
// a document that cannot be read, or that has no such section, fails rather than skips, since an
// absence here reads exactly like a pass.
func readDeploymentDoc(t *testing.T) ([]string, int) {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "deployment.md"))
	if err != nil {
		t.Fatalf("read docs/deployment.md: %v", err)
	}
	lines := strings.Split(string(text), "\n")
	start := slices.Index(lines, deploymentDocHeading)
	if start < 0 {
		t.Fatalf("docs/deployment.md has no %q heading: the settings section was renamed, and this check "+
			"stops checking the moment it cannot find it", deploymentDocHeading)
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], deploymentDocHeadingTop) {
			return lines[start:i], start + 1
		}
	}
	return lines[start:], start + 1
}

// deploymentDocTable reads the section's one markdown table: every row whose first cell is a
// dotted path in a code span, by that path. The header and the alignment row carry no code span
// and drop out. A path written twice is an error of its own, since one of the two rows would be
// read and the other silently ignored.
func deploymentDocTable(t *testing.T, section []string, firstLine int) map[string]docRow {
	t.Helper()
	rows := map[string]docRow{}
	for i, line := range section {
		if strings.HasPrefix(line, deploymentDocRefusals) {
			break
		}
		cells := deploymentDocCells(line)
		if len(cells) != 3 {
			continue
		}
		path, isPath := strings.CutPrefix(cells[0], "`")
		path, closed := strings.CutSuffix(path, "`")
		if !isPath || !closed {
			continue
		}
		// The line of the file the row stands on, for a failure an author can open.
		at := firstLine + i
		if held, twice := rows[path]; twice {
			t.Errorf("docs/deployment.md names %s twice, at lines %d and %d: one row answers and the other "+
				"is read by nobody", path, held.line, at)
			continue
		}
		rows[path] = docRow{class: cells[1], holder: strings.Trim(cells[2], "`"), line: at}
	}
	if len(rows) == 0 {
		t.Fatalf("docs/deployment.md's settings section holds no table row of the form "+
			"`| %cpath%c | class | holder |`", '`', '`')
	}
	return rows
}

// deploymentDocCells is the cells of one table row, trimmed, or nil for a line that is no row. An
// em dash, which the document writes where nothing holds a value, reads as "".
func deploymentDocCells(line string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") || !strings.HasSuffix(trimmed, "|") {
		return nil
	}
	cells := strings.Split(strings.Trim(trimmed, "|"), "|")
	for i, cell := range cells {
		cells[i] = strings.TrimSpace(cell)
		if cells[i] == "—" {
			cells[i] = ""
		}
	}
	return cells
}

// flattenDeploymentDoc is the section as one line of text with its code spans unmarked, which is
// what a refusal quoted in prose is compared against: the document breaks a long sentence across
// lines and marks a variable as code, and neither changes what the sentence says.
func flattenDeploymentDoc(section []string) string {
	return flattenDeploymentText(strings.Join(section, " "))
}

// flattenDeploymentText folds every run of whitespace into one space and drops every backtick.
func flattenDeploymentText(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "`", "")), " ")
}
