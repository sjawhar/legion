package testpg

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every database a Go file of this module creates is a copy of its source's files: a string
// literal that opens a database's creation holds `strategy file_copy` on the same line, beside it
// or in a literal after it (docs/testing.md, "A database a Go test creates"). The check reads every
// Go file of the module, tests included, since most such statements live in a test's own helper,
// so a new helper written without the clause, or an edit that drops it, fails here naming its file
// and line, with no database needed. storetest's own test measures what one clone writes.
func TestEveryCreatedDatabaseIsAFileCopy(t *testing.T) {
	if offences := creationsWithoutFileCopy(t, "../.."); len(offences) > 0 {
		t.Fatalf("%d database creations copy their source through the write-ahead log:\n\t%s\n"+
			"write `strategy file_copy` on the line that opens each one (docs/testing.md, \"A database a Go test creates\")",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// creationsWithoutFileCopy names, as file:line, every line under root that holds a string literal
// opening a database's creation and no literal holding `strategy file_copy`. Comments are not
// literals, so prose about the statement is not read.
func creationsWithoutFileCopy(t *testing.T, root string) []string {
	t.Helper()
	opening := "create" + " database" // written apart so this file holds no opening of its own
	var offences []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files := token.NewFileSet()
		file := files.AddFile(path, files.Base(), len(source))
		var lex scanner.Scanner
		lex.Init(file, source, nil, 0)
		opens, copies := map[int]bool{}, map[int]bool{}
		for {
			position, kind, literal := lex.Scan()
			if kind == token.EOF {
				break
			}
			if kind != token.STRING {
				continue
			}
			text, err := strconv.Unquote(literal)
			if err != nil {
				text = literal
			}
			text = strings.ToLower(text)
			line := files.Position(position).Line
			if strings.Contains(text, opening) {
				opens[line] = true
			}
			if strings.Contains(text, "strategy file_copy") {
				copies[line] = true
			}
		}
		for line := range opens {
			if !copies[line] {
				offences = append(offences, filepath.ToSlash(path)+":"+strconv.Itoa(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's Go files: %v", err)
	}
	slices.Sort(offences)
	return offences
}
