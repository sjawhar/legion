package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretFormCallbacksKeepExitCodesChecked(t *testing.T) {
	const source = `package main
const exitUsageError = 2
type command struct{}
type secretForm struct { help command; run func() int }
var secretForms []secretForm
func init() { secretForms = []secretForm{{command{}, good}} }
func good() int { return exitUsageError }
func dispatch() int {
	for _, form := range secretForms { return form.run() }
	return 0
}
`
	for _, tc := range []struct{ name, before, after, want string }{
		{"registered checked function", "", "", ""},
		{"undocumented handler exit", "return exitUsageError", "return 17", "exit code 17"},
		{"foreign handler", "command{}, good", "command{}, foreign.Run", "checked local exit-code function"},
		{"literal callback", "command{}, good", "command{}, func() int { return 17 }", "checked local exit-code function"},
		{"changed row", "return form.run()", "form.run = foreign.Run; return form.run()", "cannot be reassigned"},
		{"shadowed row", "return form.run()", "if true { form := foreign.Form(); return form.run() }", "exit code form.run()"},
		{"unknown callback", "range secretForms", "range otherForms", "exit code form.run()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, cliDir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			text := source
			if tc.before != "" {
				text = strings.Replace(text, tc.before, tc.after, 1)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			err := checkExitCodes(root, []docConst{{name: "exitUsageError"}})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
