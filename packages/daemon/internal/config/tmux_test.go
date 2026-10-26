package config

import (
	"path/filepath"
	"testing"
)

// runtime.tmux settles the panes' session store: the scalar `runtime: tmux`, an empty or null
// block and session_store files all keep every session a file, settling exactly as the scalar
// always has; session_store postgres carries session_dsn_file, resolved against the file's
// directory when relative and kept when absolute.
func TestRuntimeTmuxSettlesTheSessionStore(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, store string
		want                 Tmux // with a relative SessionDSNFile under the file's directory
	}{
		{"no runtime", "", "files", Tmux{}},
		{"the scalar", "runtime: tmux\n", "files", Tmux{}},
		{"an empty block", "runtime: {tmux: {}}\n", "files", Tmux{}},
		{"a null block", "runtime: {tmux: null}\n", "files", Tmux{}},
		{"files named", "runtime: {tmux: {session_store: files}}\n", "files", Tmux{}},
		{"postgres, a relative file", "runtime: {tmux: {session_store: postgres, session_dsn_file: secrets/session-dsn}}\n",
			"postgres", Tmux{SessionDSNFile: "secrets/session-dsn"}},
		{"postgres, an absolute file", "runtime:\n  tmux:\n    session_store: postgres\n    session_dsn_file: /var/run/legion/SESSION_DSN\n",
			"postgres", Tmux{SessionDSNFile: "/var/run/legion/SESSION_DSN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, minimalFile+tc.runtime)
			cfg, err := Load(path, noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := tc.want
			if want.SessionDSNFile != "" && !filepath.IsAbs(want.SessionDSNFile) {
				want.SessionDSNFile = filepath.Join(filepath.Dir(path), want.SessionDSNFile)
			}
			if cfg.Runtime != (Runtime{Name: "tmux", Tmux: want}) {
				t.Errorf("Runtime = %+v, want tmux with %+v", cfg.Runtime, want)
			}
			if cfg.Runtime.Tmux.SessionStore() != tc.store {
				t.Errorf("SessionStore() = %q, want %q", cfg.Runtime.Tmux.SessionStore(), tc.store)
			}
		})
	}
}

// Every shape runtime.tmux may not take is refused naming the key: a store of another name, the
// URL file the files store does not use, postgres without its file or with a blank one, an
// unknown key by its full dotted name, and a block that is not a mapping.
func TestRuntimeTmuxRefusesEachMalformedShapeByName(t *testing.T) {
	for _, tc := range []struct{ name, runtime, want string }{
		{"another store", "runtime: {tmux: {session_store: sqlite}}\n", "runtime.tmux.session_store must be 'files' or 'postgres'"},
		{"a store that is not a string", "runtime: {tmux: {session_store: [postgres]}}\n", "runtime.tmux.session_store must be a string"},
		{"files with a URL file", "runtime: {tmux: {session_store: files, session_dsn_file: session-dsn}}\n",
			"runtime.tmux.session_dsn_file is not used when runtime.tmux.session_store is files; remove it"},
		{"the default store with a URL file", "runtime: {tmux: {session_dsn_file: session-dsn}}\n",
			"runtime.tmux.session_dsn_file is not used when runtime.tmux.session_store is files; remove it"},
		{"postgres without a URL file", "runtime: {tmux: {session_store: postgres}}\n",
			"runtime.tmux.session_dsn_file is required with runtime.tmux.session_store postgres: the file holding the postgres:// URL of the database every pane's Oh My Pi keeps its sessions in"},
		{"postgres with a blank URL file", "runtime: {tmux: {session_store: postgres, session_dsn_file: \" \"}}\n",
			"runtime.tmux.session_dsn_file must not be empty"},
		{"an unknown key", "runtime: {tmux: {session_store: postgres, session_dsn_file: dsn, session_dsn_secret: SESSION_DSN}}\n",
			"unknown key runtime.tmux.session_dsn_secret"},
		{"a block that is not a mapping", "runtime: {tmux: postgres}\n", "runtime.tmux must be a mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.runtime), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
			}
		})
	}
}
