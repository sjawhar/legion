package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// SavedMarkerSuffix names the file the launcher writes beside a fresh Claude Code transcript once
// its first save has committed, `<transcript>.legion-saved`: claude-envoy's claim boot waits for it
// before it registers the transcript as the claim's session (packages/claude-envoy,
// src/legion-claim.ts), so the daemon never records a transcript the table does not hold, whose
// resume the next pod would refuse.
const SavedMarkerSuffix = ".legion-saved"

// saveInterval is how often the keeper looks for what a Claude Code session's files gained: a file
// is saved at most this often, and a hard kill loses at most what was written since.
const saveInterval = 2 * time.Second

// saveTimeout bounds one periodic save, its reconnection included, so a session database that hangs
// holds up only the keeper's next save and never the child.
const saveTimeout = 10 * time.Second

// tailWindow is how many of a file's last saved bytes the keeper holds: a file that still has them
// where they were saved, in the same inode, is taken as an extension of what was saved and only its
// new bytes are appended. Claude Code appends to its transcript and replaces its phase-reminder
// record by a rename, so anything else is a file the keeper rewrites whole.
const tailWindow = 4096

// resumeLookupTimeout bounds the session table lookup a resume's start waits on, retries included;
// a test shortens it. A start's whole budget is the proof dial, at most proofTimeout (StopGrace/2),
// then this lookup: 35 s at the daemon's defaults, inside its wait on the start, its boot timeout
// plus its stop grace (120 s + 10 s: runtime/sandbox's relaunch), whenever the launcher is connected
// as that wait begins, which is every relaunch into a running pod. Only a launcher that connects late
// in a new pod's boot window has less: at least the grace remains, and the proof's half of it is
// still inside.
var resumeLookupTimeout = 30 * time.Second

// resumeLookupBackoff is the wait before each retry of a session table lookup that could not reach
// the database, doubling up to its last value until resumeLookupTimeout; a test shortens it.
var resumeLookupBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// sessionDial is how the launcher reaches a generation's session database, for a resume's lookup
// (sessionWritten) and for the keeper alike: through clientURL, the generation's own database
// through the socket its proxy has already bound, or with none through the URL file the child's
// environment names (DSNFileVariable), read afresh at each connection, since that file is a
// projection something else may replace.
type sessionDial struct{ clientURL, dsnFile string }

// newSessionDial is the session database a generation's environment names, refused when it has no
// database of its own and names no URL file.
func newSessionDial(env []string, clientURL string) (sessionDial, error) {
	if clientURL != "" {
		return sessionDial{clientURL: clientURL}, nil
	}
	if file := strings.TrimSpace(envValue(env, ompsessions.DSNFileVariable)); file != "" {
		return sessionDial{dsnFile: file}, nil
	}
	return sessionDial{}, fmt.Errorf("%s names no file", ompsessions.DSNFileVariable)
}

// url is the database's URL: clientURL, or what the URL file holds now.
func (d sessionDial) url() (string, error) {
	if d.dsnFile == "" {
		return d.clientURL, nil
	}
	return ompsessions.ReadDSN(d.dsnFile)
}

// connect opens one connection to the database.
func (d sessionDial) connect(ctx context.Context) (*pgx.Conn, error) {
	dsn, err := d.url()
	if err != nil {
		return nil, err
	}
	return ompsessions.ConnectURL(ctx, dsn)
}

// dial opens a connection on which ready succeeds, the caller's to close. One that cannot be
// opened, or on which ready fails, is asked again after each of resumeLookupBackoff's waits until
// resumeLookupTimeout, as a database that is briefly unreachable must not fail the launch; a URL
// file that is missing or empty is refused at once. parent's end (the container's stop) ends the
// wait as the timeout does.
func (d sessionDial) dial(parent context.Context, ready func(context.Context, *pgx.Conn) error) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(parent, resumeLookupTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		dsn, err := d.url()
		if err != nil {
			return nil, err
		}
		conn, err := ompsessions.ConnectURL(ctx, dsn)
		if err == nil {
			if err = ready(ctx, conn); err == nil {
				return conn, nil
			}
			closeSessions(conn)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%v (after %d attempts in %s)", err, attempt+1, resumeLookupTimeout)
		case <-time.After(resumeLookupBackoff[min(attempt, len(resumeLookupBackoff)-1)]):
		}
	}
}

// closeSessions closes conn, a session database connection if any, bounded by the proxy's own
// drain.
func closeSessions(conn *pgx.Conn) {
	if conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), sessionDrain)
		_ = conn.Close(ctx)
		cancel()
	}
}

// claudeKeeper keeps a Claude Code generation's session in Oh My Pi's session table, since the
// pod's disk, which holds the session under CLAUDE_CONFIG_DIR, dies with the pod
// (claudelaunch.SessionVariable). Two files are kept, each a row keyed by its absolute path: the
// transcript, `<CLAUDE_CONFIG_DIR>/projects/<dir>/<id>.jsonl`, and the phase-reminder record,
// `<CLAUDE_CONFIG_DIR>/legion-phase-stall-<id>.json` (packages/claude-envoy,
// src/legion-phase-hook.ts). It holds one connection to the session database for the generation's
// life, the one a resume's lookup made or one reached as that lookup reaches it (sessionDial): the
// generation's proxy socket under cloudsql-iam, the URL file the child is pointed at otherwise. A
// connection that breaks is opened again at the next save.
type claudeKeeper struct {
	log       io.Writer
	sessions  sessionDial
	configDir string
	session   string
	conn      *pgx.Conn
	// transcript and record are the two kept files: the transcript and the phase-reminder record.
	transcript, record *keptFile
	// appendRow and replaceRow write a kept file's row: ompsessions.Append and Replace, which a
	// test wraps to fail.
	appendRow, replaceRow rowWrite
	// ctx ends a periodic save still under way when the final one can wait no longer (finish).
	ctx       context.Context
	interrupt context.CancelFunc
	quit      chan struct{}
	done      chan struct{}
}

// rowWrite is ompsessions.Append's and Replace's shape: a compare-and-set write of content at path
// over the byte_len its writer last saved.
type rowWrite func(ctx context.Context, conn *pgx.Conn, path string, saved int64, content []byte, mtime time.Time) error

// keptFile is one kept file as its writer last saved it.
type keptFile struct {
	// path is "" for a fresh transcript Claude Code has not written yet.
	path string
	// lines is whether the file is saved only up to its last newline, so a row never ends in half
	// a line: the transcript's.
	lines bool
	// marker is whether the next committed save writes SavedMarkerSuffix beside the file: a fresh
	// transcript's first.
	marker bool
	// extends is whether the file at (dev, ino) holds the saved bytes, ending in tail.
	extends bool
	keptState
	// pending are the saves since the last committed one whose commit went unanswered: each may
	// have landed, and a refusal that the row moved to what one of them wrote is that one landing
	// (adopt).
	pending []pendingSave
	// stopped is a writer whose row moved under it: it saves nothing more, so it never overwrites
	// what another writer saved.
	stopped bool
}

// pendingSave is a save that may have landed: the row's byte_len it leaves, and the sha256 of what
// it wrote from offset from, the old byte_len for an append and 0 for a whole row, with the file it
// read them from.
type pendingSave struct {
	saved, from int64
	sum         [sha256.Size]byte
	dev, ino    uint64
}

// keptState is what a committed save leaves a keptFile holding.
type keptState struct {
	// saved is the row's byte_len as this writer last committed it, ompsessions.Absent for none.
	saved int64
	// dev and ino are the file the saved bytes were read from, and tail their last tailWindow.
	dev, ino uint64
	tail     []byte
	// size and mtime are the file as the keeper last finished with it, so an unchanged file is
	// not read again.
	size  int64
	mtime time.Time
}

// newClaudeKeeper is the keeper of the Claude Code session a generation's environment names, nil
// for a generation that names none (every Oh My Pi launch and every tmux one) or whose sessions are
// not in a database. It checks the session's paths and connects nothing: a resume's transcript is
// resume, which must be `<CLAUDE_CONFIG_DIR>/projects/<dir>/<id>.jsonl` for the session's own id
// (claudelaunch.ResumeSession), and a fresh one is found once Claude Code writes it (find).
func newClaudeKeeper(env []string, resume, clientURL string, log io.Writer) (*claudeKeeper, error) {
	session := envValue(env, claudelaunch.SessionVariable)
	if session == "" || envValue(env, ompsessions.StorageVariable) != ompsessions.SQLStorage {
		return nil, nil
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("keep Claude Code session %s: "+format, append([]any{session}, args...)...)
	}
	configDir := envValue(env, "CLAUDE_CONFIG_DIR")
	if !filepath.IsAbs(configDir) {
		return nil, refuse("CLAUDE_CONFIG_DIR %q is no absolute directory", configDir)
	}
	configDir = filepath.Clean(configDir)
	// The id names both files, so it must be one: a uuid, with no separator or dot to lead either
	// outside the directory.
	if !claudelaunch.IsSessionID(session) {
		return nil, refuse("%s is no Claude Code session id", claudelaunch.SessionVariable)
	}
	sessions, err := newSessionDial(env, clientURL)
	if err != nil {
		return nil, refuse("%v", err)
	}
	k := &claudeKeeper{log: log, sessions: sessions, configDir: configDir, session: session,
		appendRow: ompsessions.Append, replaceRow: ompsessions.Replace, quit: make(chan struct{}), done: make(chan struct{}),
		transcript: &keptFile{lines: true, marker: resume == "", keptState: keptState{saved: ompsessions.Absent}},
		record:     &keptFile{path: filepath.Join(configDir, "legion-phase-stall-"+session+".json"), keptState: keptState{saved: ompsessions.Absent}}}
	if resume != "" {
		id, err := claudelaunch.ResumeSession(configDir, resume)
		if err != nil {
			return nil, refuse("%v", err)
		}
		if id != session {
			return nil, refuse("the resumed transcript %s is session %s's", resume, id)
		}
		k.transcript.path = filepath.Clean(resume)
	}
	k.ctx, k.interrupt = context.WithCancel(context.Background())
	return k, nil
}

// open readies the keeper before the child starts, on conn, the connection a resume's lookup made
// (resumable), or on one it dials as that lookup does (sessionDial.dial) when it has none or conn
// cannot make Oh My Pi's tables, which open makes where the database has none. A resume, whose
// transcript the keeper names from the start, then restores each file from its row (restore), so
// the child starts on the session the table holds, and the transcript's marker is written: the
// table already holds what the claim records. parent's end (the container's stop) ends the wait as
// the timeout does. A nil keeper, a generation that keeps nothing, closes conn.
func (k *claudeKeeper) open(parent context.Context, conn *pgx.Conn) error {
	if k == nil {
		closeSessions(conn)
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, resumeLookupTimeout)
	defer cancel()
	if conn != nil && ompsessions.CreateTables(ctx, conn) != nil {
		closeSessions(conn)
		conn = nil
	}
	if conn == nil {
		var err error
		if conn, err = k.sessions.dial(ctx, ompsessions.CreateTables); err != nil {
			return fmt.Errorf("keep Claude Code session %s: %v", k.session, err)
		}
	}
	k.conn = conn
	if k.transcript.path == "" {
		return nil
	}
	for _, f := range [...]*keptFile{k.transcript, k.record} {
		if err := k.restore(ctx, f); err != nil {
			return fmt.Errorf("keep Claude Code session %s: restore %s: %v", k.session, f.path, err)
		}
	}
	return writeMarker(k.transcript.path)
}

// connect opens the keeper's connection when it has none open, and makes Oh My Pi's tables.
func (k *claudeKeeper) connect(ctx context.Context) error {
	if k.conn != nil && !k.conn.IsClosed() {
		return nil
	}
	conn, err := k.sessions.connect(ctx)
	if err != nil {
		return err
	}
	if err := ompsessions.CreateTables(ctx, conn); err != nil {
		closeSessions(conn)
		return err
	}
	k.conn = conn
	return nil
}

// restore writes f's row to f's path, making its directories, when the file there is missing or a
// strict prefix of the row. It never shortens a longer file: a relaunch into the same pod keeps
// what the last save missed, which the first save then appends. A file that is neither a prefix
// nor an extension of the row is left as it is, and the first save rewrites the row from it.
func (k *claudeKeeper) restore(ctx context.Context, f *keptFile) error {
	row, ok, err := ompsessions.Read(ctx, k.conn, f.path)
	if err != nil || !ok {
		return err
	}
	f.saved, f.tail = int64(len(row)), lastWindow(row)
	local, err := readNoFollow(f.path)
	switch {
	case errors.Is(err, os.ErrNotExist) || err == nil && len(local) < len(row) && bytes.HasPrefix(row, local):
		if err := writeRestored(f.path, row); err != nil {
			return err
		}
	case err != nil:
		return err
	case !bytes.HasPrefix(local, row):
		return nil
	}
	info, err := os.Lstat(f.path)
	if err != nil {
		return err
	}
	f.extends = true
	f.dev, f.ino = fileID(info)
	return nil
}

// run saves the session's files every saveInterval until finish. A nil keeper saves nothing.
func (k *claudeKeeper) run() {
	if k == nil {
		return
	}
	defer close(k.done)
	ticker := time.NewTicker(saveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-k.quit:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(k.ctx, saveTimeout)
			k.save(ctx)
			cancel()
		}
	}
}

// finish ends the keeping once the child has exited: the periodic saves stop, one last save runs
// until deadline, and the connection closes. A periodic save still under way at deadline is ended,
// and the last committed one stands. Best effort: a deadline already past saves nothing more. A nil
// keeper has nothing to finish.
func (k *claudeKeeper) finish(deadline time.Time) {
	if k == nil {
		return
	}
	close(k.quit)
	select {
	case <-k.done:
	case <-time.After(time.Until(deadline)):
		k.interrupt()
		<-k.done
	}
	if time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		k.save(ctx)
		cancel()
	}
	k.close()
}

// close closes the keeper's connection, bounded by the proxy's own drain (closeSessions). A nil
// keeper has none.
func (k *claudeKeeper) close() {
	if k == nil {
		return
	}
	k.interrupt()
	closeSessions(k.conn)
}

// save saves each kept file that changed since its writer last finished with it. A save Postgres
// refuses, or a database it cannot reach, is logged and tried again at the next save; a row that
// moved under its writer stops that writer, logged.
func (k *claudeKeeper) save(ctx context.Context) {
	for _, f := range [...]*keptFile{k.transcript, k.record} {
		if f.stopped {
			continue
		}
		if f.path == "" {
			if f.path = k.find(); f.path == "" {
				continue
			}
		}
		if err := k.saveFile(ctx, f); err != nil {
			var moved *ompsessions.MovedError
			if errors.As(err, &moved) {
				f.stopped = true
				k.logf("stops saving %s for the rest of the generation: %v; nothing of it is overwritten", f.path, err)
				continue
			}
			k.logf("could not save %s: %v; it is tried again at the next save", f.path, err)
		}
	}
}

// find is the fresh transcript Claude Code wrote for the session, found as claude-envoy finds it
// (waitForTranscript, src/legion-claim.ts): `projects/<dir>/<id>.jsonl` under any directory, never
// by Claude Code's own rule for naming that directory. "" until it exists.
func (k *claudeKeeper) find() string {
	projects := filepath.Join(k.configDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		file := filepath.Join(projects, entry.Name(), k.session+".jsonl")
		if info, err := os.Lstat(file); err == nil && info.Mode().IsRegular() {
			return file
		}
	}
	return ""
}

// saveFile saves what f gained since its writer last saved it (saveChanges). A refusal that the
// row moved to what one of f's pending saves wrote is that save landing: f takes it as committed
// (adopt) and saves again from there at once, so nothing waits on a next save that, after finish's
// last one, never comes.
func (k *claudeKeeper) saveFile(ctx context.Context, f *keptFile) error {
	err := k.saveChanges(ctx, f)
	var moved *ompsessions.MovedError
	if !errors.As(err, &moved) {
		return err
	}
	adopted, readErr := k.adopt(ctx, f, moved.Held)
	switch {
	case readErr != nil:
		return fmt.Errorf("read the row it moved to: %w", readErr)
	case adopted:
		return k.saveChanges(ctx, f)
	}
	return err
}

// saveChanges saves what f gained since its writer last saved it: only its new bytes, appended,
// when the file still holds what was saved, and the whole of it otherwise. A transcript is saved up
// to its last newline. A file not written yet, or with nothing new to save, writes nothing.
func (k *claudeKeeper) saveChanges(ctx context.Context, f *keptFile) error {
	file, err := os.OpenFile(f.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	dev, ino := fileID(info)
	same := f.extends && dev == f.dev && ino == f.ino
	if same && info.Size() == f.size && info.ModTime().Equal(f.mtime) {
		return nil
	}
	if err := k.connect(ctx); err != nil {
		return err
	}
	state := keptState{saved: f.saved, dev: dev, ino: ino, tail: f.tail, size: info.Size(), mtime: info.ModTime()}
	if same && info.Size() >= f.saved {
		from := f.saved - int64(len(f.tail))
		window := make([]byte, info.Size()-from)
		if _, err := file.ReadAt(window, from); err != nil {
			return err
		}
		if bytes.Equal(window[:len(f.tail)], f.tail) {
			added := window[len(f.tail):]
			if f.lines {
				added = added[:bytes.LastIndexByte(added, '\n')+1]
			}
			if len(added) == 0 {
				k.hold(f, state)
				return nil
			}
			state.saved = f.saved + int64(len(added))
			state.tail = lastWindow(window[:len(f.tail)+len(added)])
			return k.saveRow(ctx, f, state, true, added)
		}
	}
	content := make([]byte, info.Size())
	if _, err := file.ReadAt(content, 0); err != nil {
		return err
	}
	if f.lines {
		content = content[:bytes.LastIndexByte(content, '\n')+1]
	}
	if len(content) == 0 {
		return nil
	}
	state.saved, state.tail = int64(len(content)), lastWindow(content)
	return k.saveRow(ctx, f, state, false, content)
}

// saveRow writes content as f's row, appended to what f last saved or as the whole of it, and
// takes the outcome: committed, f holds state; unanswered by anything but a refusal that the row
// moved, the save may have landed and is pending until a save commits (adopt).
func (k *claudeKeeper) saveRow(ctx context.Context, f *keptFile, state keptState, appended bool, content []byte) error {
	write, from := k.replaceRow, int64(0)
	if appended {
		write, from = k.appendRow, f.saved
	}
	err := write(ctx, k.conn, f.path, f.saved, content, state.mtime)
	var moved *ompsessions.MovedError
	switch {
	case err == nil:
		k.commit(f, state)
	case !errors.As(err, &moved):
		pending := pendingSave{saved: state.saved, from: from, sum: sha256.Sum256(content), dev: state.dev, ino: state.ino}
		if !slices.Contains(f.pending, pending) {
			f.pending = append(f.pending, pending)
		}
	}
	return err
}

// adopt takes the row at f's path, which the table holds at held bytes, as committed when one of
// f's pending saves wrote it: the row is as long as that save left it and holds what it wrote
// where it wrote it. A length alone tells neither two pending saves of one length apart nor a
// pending save from another writer's row of that length; the bytes do. The row is read only when
// some pending save left held bytes, and any row none of them wrote is another writer's: false, and
// f stops (save).
func (k *claudeKeeper) adopt(ctx context.Context, f *keptFile, held int64) (bool, error) {
	if !slices.ContainsFunc(f.pending, func(p pendingSave) bool { return p.saved == held }) {
		return false, nil
	}
	row, ok, err := ompsessions.Read(ctx, k.conn, f.path)
	if err != nil || !ok {
		return false, err
	}
	for _, p := range f.pending {
		if p.saved == int64(len(row)) && sha256.Sum256(row[p.from:]) == p.sum {
			// size -1 has the next save look at the file again, whatever it read last.
			k.commit(f, keptState{saved: p.saved, dev: p.dev, ino: p.ino, tail: lastWindow(row), size: -1})
			return true, nil
		}
	}
	return false, nil
}

// commit makes f hold state, a committed save's. The saves pending before it either landed before
// it, or it would have been refused, or never land, and are pending no more.
func (k *claudeKeeper) commit(f *keptFile, state keptState) {
	f.pending = nil
	k.hold(f, state)
}

// hold makes f hold state, and writes the marker it is owed.
func (k *claudeKeeper) hold(f *keptFile, state keptState) {
	f.keptState, f.extends = state, true
	if f.marker && f.saved > 0 {
		if err := writeMarker(f.path); err != nil {
			k.logf("could not mark %s saved: %v; it is tried again at the next save", f.path, err)
			f.size = -1
			return
		}
		f.marker = false
	}
}

func (k *claudeKeeper) logf(format string, args ...any) {
	if k.log != nil {
		fmt.Fprintf(k.log, "legion launcher: Claude Code session %s: "+format+"\n", append([]any{k.session}, args...)...)
	}
}

// writeMarker writes SavedMarkerSuffix beside transcript, an empty owner-only file, never through
// a link already at that path.
func writeMarker(transcript string) error {
	marker, err := os.OpenFile(transcript+SavedMarkerSuffix, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return marker.Close()
}

// writeRestored writes content to path whole: into a fresh file beside it, renamed over it, so a
// restore cut short leaves the file as it was and never follows a link at path.
func writeRestored(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".legion-restore-*")
	if err != nil {
		return err
	}
	_, err = staged.Write(content)
	if closeErr := staged.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(staged.Name(), path)
	}
	if err != nil {
		_ = os.Remove(staged.Name())
	}
	return err
}

// readNoFollow is path's content, refusing a link at path.
func readNoFollow(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// lastWindow is content's last tailWindow bytes, copied.
func lastWindow(content []byte) []byte {
	return bytes.Clone(content[max(0, len(content)-tailWindow):])
}

func fileID(info os.FileInfo) (dev, ino uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), stat.Ino
	}
	return 0, 0
}
