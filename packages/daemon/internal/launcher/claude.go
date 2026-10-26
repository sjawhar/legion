package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// claudeKeeper keeps a Claude Code generation's session in Oh My Pi's session table, since the
// pod's disk, which holds the session under CLAUDE_CONFIG_DIR, dies with the pod
// (claudelaunch.SessionVariable). Two files are kept, each a row keyed by its absolute path: the
// transcript, `<CLAUDE_CONFIG_DIR>/projects/<dir>/<id>.jsonl`, and the phase-reminder record,
// `<CLAUDE_CONFIG_DIR>/legion-phase-stall-<id>.json` (packages/claude-envoy,
// src/legion-phase-hook.ts). It holds one connection to the session database for the generation's
// life, reached as the launcher's own resume lookup reaches it (sessionWritten): the generation's
// proxy socket under cloudsql-iam, the URL file the child is pointed at otherwise. A connection
// that breaks is opened again at the next save.
type claudeKeeper struct {
	log       io.Writer
	clientURL string
	dsnFile   string
	configDir string
	session   string
	conn      *pgx.Conn
	// transcript is files[0], the phase-reminder record files[1].
	files []*keptFile
	// ctx ends a periodic save still under way when the final one can wait no longer (finish).
	ctx       context.Context
	interrupt context.CancelFunc
	quit      chan struct{}
	done      chan struct{}
}

// keptFile is one kept file as its writer last saved it.
type keptFile struct {
	// path is "" for a fresh transcript Claude Code has not written yet.
	path string
	// transcript is whether the file is the transcript, saved only up to its last newline so a
	// row never ends in half a line.
	transcript bool
	// marker is whether the next committed save writes SavedMarkerSuffix beside the file: a fresh
	// transcript's first.
	marker bool
	// extends is whether the file at (dev, ino) holds the saved bytes, ending in tail.
	extends bool
	keptState
	// unacked is a save whose commit failed to answer: it may have landed. A refusal that the row
	// moved to exactly its length is that save landing, and is taken as committed.
	unacked *keptState
	// stopped is a writer whose row moved under it: it saves nothing more, so it never overwrites
	// what another writer saved.
	stopped bool
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
	k := &claudeKeeper{log: log, clientURL: clientURL, configDir: configDir, session: session,
		quit: make(chan struct{}), done: make(chan struct{})}
	if clientURL == "" {
		if k.dsnFile = strings.TrimSpace(envValue(env, ompsessions.DSNFileVariable)); k.dsnFile == "" {
			return nil, refuse("%s names no file", ompsessions.DSNFileVariable)
		}
	}
	transcript := &keptFile{transcript: true, marker: resume == "", keptState: keptState{saved: ompsessions.Absent}}
	if resume != "" {
		id, err := claudelaunch.ResumeSession(configDir, resume)
		if err != nil {
			return nil, refuse("%v", err)
		}
		if id != session {
			return nil, refuse("the resumed transcript %s is session %s's", resume, id)
		}
		transcript.path = filepath.Clean(resume)
	}
	record := &keptFile{path: filepath.Join(configDir, "legion-phase-stall-"+session+".json"), keptState: keptState{saved: ompsessions.Absent}}
	k.files = []*keptFile{transcript, record}
	k.ctx, k.interrupt = context.WithCancel(context.Background())
	return k, nil
}

// open connects the keeper before the child starts, asking again after each of
// resumeLookupBackoff's waits until resumeLookupTimeout as a resume's lookup does, and makes Oh My
// Pi's tables where the database has none. A resume then restores each file from its row
// (restore), so the child starts on the session the table holds, and the transcript's marker is
// written: the table already holds what the claim records. parent's end (the container's stop)
// ends the wait as the timeout does.
func (k *claudeKeeper) open(parent context.Context, resume bool) error {
	ctx, cancel := context.WithTimeout(parent, resumeLookupTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		err := k.connect(ctx)
		if err == nil {
			break
		}
		wait := resumeLookupBackoff[min(attempt, len(resumeLookupBackoff)-1)]
		select {
		case <-ctx.Done():
			return fmt.Errorf("keep Claude Code session %s: %v (after %d attempts in %s)", k.session, err, attempt+1, resumeLookupTimeout)
		case <-time.After(wait):
		}
	}
	if !resume {
		return nil
	}
	for _, f := range k.files {
		if err := k.restore(ctx, f); err != nil {
			return fmt.Errorf("keep Claude Code session %s: restore %s: %v", k.session, f.path, err)
		}
	}
	return writeMarker(k.files[0].path)
}

// connect opens the keeper's connection when it has none open, and makes Oh My Pi's tables.
func (k *claudeKeeper) connect(ctx context.Context) error {
	if k.conn != nil && !k.conn.IsClosed() {
		return nil
	}
	var conn *pgx.Conn
	var err error
	if k.clientURL != "" {
		conn, err = ompsessions.ConnectURL(ctx, k.clientURL)
	} else {
		conn, err = ompsessions.Connect(ctx, k.dsnFile)
	}
	if err != nil {
		return err
	}
	if err := ompsessions.CreateTables(ctx, conn); err != nil {
		_ = conn.Close(context.Background())
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

// run saves the session's files every saveInterval until finish.
func (k *claudeKeeper) run() {
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
// and the last committed one stands. Best effort: a deadline already past saves nothing more.
func (k *claudeKeeper) finish(deadline time.Time) {
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

// close closes the keeper's connection, bounded by the proxy's own drain.
func (k *claudeKeeper) close() {
	k.interrupt()
	if k.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), sessionDrain)
		_ = k.conn.Close(ctx)
		cancel()
	}
}

// save saves each kept file that changed since its writer last finished with it. A save Postgres
// refuses, or a database it cannot reach, is logged and tried again at the next save; a row that
// moved under its writer stops that writer, logged.
func (k *claudeKeeper) save(ctx context.Context) {
	for _, f := range k.files {
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

// saveFile saves what f gained since its writer last saved it: only its new bytes, appended, when
// the file still holds what was saved, and the whole of it otherwise. A transcript is saved up to
// its last newline. A file not written yet, or with nothing new to save, writes nothing.
func (k *claudeKeeper) saveFile(ctx context.Context, f *keptFile) error {
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
			if f.transcript {
				added = added[:bytes.LastIndexByte(added, '\n')+1]
			}
			if len(added) > 0 {
				state.saved = f.saved + int64(len(added))
				state.tail = lastWindow(window[:len(f.tail)+len(added)])
				err = ompsessions.Append(ctx, k.conn, f.path, f.saved, added, info.ModTime())
			}
			return k.settle(f, state, err)
		}
	}
	content := make([]byte, info.Size())
	if _, err := file.ReadAt(content, 0); err != nil {
		return err
	}
	if f.transcript {
		content = content[:bytes.LastIndexByte(content, '\n')+1]
	}
	if len(content) == 0 {
		return nil
	}
	state.saved, state.tail = int64(len(content)), lastWindow(content)
	return k.settle(f, state, ompsessions.Replace(ctx, k.conn, f.path, f.saved, content, info.ModTime()))
}

// settle takes a save's outcome: committed, f holds what it saved, and a fresh transcript's first
// commit writes its marker. A save that may have landed unanswered is remembered (unacked), and a
// refusal that the row moved to exactly that save's length is that save landing.
func (k *claudeKeeper) settle(f *keptFile, state keptState, err error) error {
	var moved *ompsessions.MovedError
	if errors.As(err, &moved) && f.unacked != nil && moved.Held == f.unacked.saved {
		k.commit(f, *f.unacked)
		f.size = -1 // look again at the next save, which saves what this one did not
		return nil
	}
	f.unacked = nil
	if err != nil {
		if !errors.As(err, &moved) && state.saved != f.saved {
			f.unacked = &state
		}
		return err
	}
	k.commit(f, state)
	return nil
}

// commit makes f hold state, a committed save's, and writes the marker it is owed.
func (k *claudeKeeper) commit(f *keptFile, state keptState) {
	f.keptState, f.extends, f.unacked = state, true, nil
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
