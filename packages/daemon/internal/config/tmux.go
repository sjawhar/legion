package config

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	tmuxKey               = "runtime.tmux"
	tmuxSessionStoreKey   = tmuxKey + ".session_store"
	tmuxSessionDSNFileKey = tmuxKey + ".session_dsn_file"
	tmuxSessionFiles      = "files"
	tmuxSessionPostgres   = "postgres"
)

// Tmux is the settled `runtime.tmux` block, under runtime tmux alone. Its zero value, which the
// scalar `runtime: tmux` settles to, keeps every Oh My Pi session a file on the daemon's own host
// (session_store files, the default).
type Tmux struct {
	// SessionDSNFile is runtime.tmux.session_dsn_file resolved against the file's directory: under
	// session_store postgres, the file holding the postgres:// URL of the database every pane's Oh
	// My Pi keeps its sessions in, which each pane is told as OMP_SESSION_SQL_DSN_FILE and the
	// daemon reads its conversations through; "" under session_store files.
	SessionDSNFile string
}

// InDatabase is whether every pane's Oh My Pi keeps its sessions as rows of the database
// SessionDSNFile names rather than as files (session_store postgres).
func (t Tmux) InDatabase() bool { return t.SessionDSNFile != "" }

// SessionStore is runtime.tmux.session_store as the file spells it: "postgres" or "files".
func (t Tmux) SessionStore() string {
	if t.InDatabase() {
		return tmuxSessionPostgres
	}
	return tmuxSessionFiles
}

// readTmux reads the `runtime.tmux` mapping: `session_store`, files by default, with
// `session_dsn_file`, the one key postgres carries and files refuses, as runtime.kubernetes'
// session keys do. A null block is the default store. The file is kept as written; resolve settles
// it against the file's directory.
func readTmux(value *yaml.Node) (Tmux, error) {
	if value.Tag == "!!null" {
		return Tmux{}, nil
	}
	if value.Kind != yaml.MappingNode {
		return Tmux{}, errors.New(tmuxKey + " must be a mapping")
	}
	fields, err := members(value, tmuxKey, "session_store", "session_dsn_file")
	if err != nil {
		return Tmux{}, err
	}
	store, err := readString(fields["session_store"], tmuxSessionStoreKey)
	if err != nil {
		return Tmux{}, err
	}
	kind := tmuxSessionFiles
	if store != nil {
		kind = *store
	}
	switch kind {
	case tmuxSessionFiles:
		if fields["session_dsn_file"] != nil {
			return Tmux{}, fmt.Errorf("%s is not used when %s is %s; remove it", tmuxSessionDSNFileKey, tmuxSessionStoreKey, kind)
		}
		return Tmux{}, nil
	case tmuxSessionPostgres:
		file, err := requiredString(fields["session_dsn_file"], tmuxSessionDSNFileKey,
			" with "+tmuxSessionStoreKey+" postgres: the file holding the postgres:// URL of the database every pane's Oh My Pi keeps its sessions in")
		if err != nil {
			return Tmux{}, err
		}
		return Tmux{SessionDSNFile: file}, nil
	}
	return Tmux{}, errors.New(tmuxSessionStoreKey + " must be 'files' or 'postgres'")
}
