package ompsessions

import (
	"net/url"
	"strings"
	"unicode"

	"cloud.google.com/go/cloudsqlconn/instance"
)

// StoreKind is which session store a deployment runs (runtime.kubernetes.session_store).
type StoreKind string

const (
	// StoreVolume keeps every session as a file on the issue's volume.
	StoreVolume StoreKind = "pvc"
	// StorePostgres keeps every session in the database whose whole postgres:// URL, password and
	// all, one providers Secret key holds.
	StorePostgres StoreKind = "postgres"
	// StoreCloudSQLIAM keeps every session in a Cloud SQL database each pod signs in to as itself,
	// with no password written anywhere: a role container's launcher binds a unix socket of its own
	// and proxies it to the instance over the pod's cloud identity (internal/sessionproxy).
	StoreCloudSQLIAM StoreKind = "cloudsql-iam"
)

// Store is the settled session store: the kind, and the one field that kind alone carries, so no
// reader can reach a database without what opens it. The zero value is StoreVolume's: sessions are
// files.
type Store struct {
	Kind StoreKind
	// DSNSecret is the providers Secret's key that holds the session database's whole postgres://
	// URL, under StorePostgres alone.
	DSNSecret string
	// Database is the Cloud SQL database every pod signs in to as itself, under StoreCloudSQLIAM
	// alone.
	Database Database
}

// Database is a Cloud SQL session database: the instance's connection name (ValidInstance), the
// database in it, and the IAM login a pod signs in as.
type Database struct{ Instance, Name, User string }

// ValidInstance reports whether name is a Cloud SQL instance's connection name the connector
// uses whole: it holds no Unicode whitespace, the Cloud SQL connector's own parser
// (cloudsqlconn's instance.ParseConnName) accepts it, and the project, region and name it parses
// out, joined again as project:region:name, are name itself. The connector takes
// project:region:instance, or domain:project:region:instance for a legacy domain-scoped project,
// but its pattern is unanchored and would quietly take a part of a longer string (a:b:c:d:e parses
// as project a:b, region c, name d), so the round trip refuses whatever it would not use whole.
// It is the one rule, which both the loader that reads the instance from the file and the proxy
// that dials it hold to, so --check-config accepts nothing the connector would refuse or read
// otherwise.
func ValidInstance(name string) bool {
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return false
	}
	c, err := instance.ParseConnName(name)
	return err == nil && c.Project()+":"+c.Region()+":"+c.Name() == name
}

// InDatabase is whether a session is a row rather than a file, which both database kinds are: then
// nothing a volume holds says anything about a session.
func (s Store) InDatabase() bool { return s.Kind == StorePostgres || s.Kind == StoreCloudSQLIAM }

// DSNSecretKey is the providers Secret's key that holds the session database's URL, "" unless the
// store is the postgres one: under cloudsql-iam no pod mounts a session key, since each pod signs
// in to the instance as itself and there is no URL to keep anywhere.
func (s Store) DSNSecretKey() string {
	if s.Kind != StorePostgres {
		return ""
	}
	return s.DSNSecret
}

// SocketName is libpq's own name for a unix socket of a Postgres server on port 5432. pgx and
// libpq address such a socket by its directory and compose this name themselves, where Bun's
// client is handed the socket whole, so the name must be stated here for the agent's spelling
// (AgentURL) and for the launcher that binds it (Socket).
const SocketName = ".s.PGSQL.5432"

// DSNFileName is the file DSNFileVariable points at: the providers mount's projection of the
// session database's URL under StorePostgres, and the file a generation's own launcher writes in
// its private directory under StoreCloudSQLIAM.
const DSNFileName = "OMP_SESSION_SQL_DSN"

// Endpoint is one unix-socket session database as its two clients address it: the directory the
// socket is bound in, the database, and the login. A login is an IAM login and holds an `@`, so
// both spellings are built with net/url, which percent-encodes it wherever it lands.
type Endpoint struct{ SocketDir, Database, User string }

// Socket is the socket's path: what the launcher binds, and what AgentURL names.
func (e Endpoint) Socket() string { return e.SocketDir + "/" + SocketName }

// AgentURL is the connection URL Oh My Pi's client reads from the DSN file. Oh My Pi hands that
// text to Bun's SQL client, which takes a unix socket as the URL's `path` parameter and ignores the
// host, then sends a plain Postgres startup packet carrying the user and the database and no
// password — which is the whole packet the proxy's upstream expects, the login travelling in the
// connector's certificate exchange instead.
func (e Endpoint) AgentURL() string {
	u := url.URL{Scheme: "postgres", User: url.User(e.User), Host: "localhost", Path: "/" + e.Database}
	u.RawQuery = url.Values{"path": {e.Socket()}, "sslmode": {"disable"}}.Encode()
	return u.String()
}

// ClientURL is the same endpoint for the launcher's own pgx client. The two spellings are not
// interchangeable: pgx takes libpq's `host` as the socket's directory and appends SocketName
// itself, where AgentURL names the socket whole.
func (e Endpoint) ClientURL() string {
	u := url.URL{Scheme: "postgres", Path: "/" + e.Database}
	u.RawQuery = url.Values{"host": {e.SocketDir}, "sslmode": {"disable"}, "user": {e.User}}.Encode()
	return u.String()
}
