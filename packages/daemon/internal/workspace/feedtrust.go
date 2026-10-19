package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// ownFeedRepository makes the feed repository gitDir one Fetch may take as its own, before it runs
// any git there: a symlink at the repository's owner directory is removed (an earlier release's
// agent could have pointed it anywhere), the stale locks a killed fetch left are removed
// (RemoveStaleFeedLocks, which follows no link), and a repository foreignFeed refuses is removed
// whole, logged naming why. It answers whether a repository Fetch may fetch into is left there.
func ownFeedRepository(gitDir string, repo ghrepo.Repository, now time.Time, log func(string)) (bool, error) {
	owner := filepath.Dir(gitDir)
	if info, err := os.Lstat(owner); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if err := os.Remove(owner); err != nil {
			return false, fmt.Errorf("remove the symlink at the feed's %s: %w", owner, err)
		}
		log(fmt.Sprintf("the feed's %s is not one this fetch wrote (it is a symlink): removed; the feed is filled afresh", owner))
		return false, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(gitDir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := RemoveStaleFeedLocks(gitDir, now, log); err != nil {
		return false, err
	}
	why, err := foreignFeed(gitDir)
	if err != nil {
		return false, err
	}
	if why == "" {
		return true, nil
	}
	if err := os.RemoveAll(gitDir); err != nil {
		return false, fmt.Errorf("remove the feed's %s, which %s: %w", gitDir, why, err)
	}
	log(fmt.Sprintf("the feed's %s is not one this fetch wrote (%s): removed; the feed is filled afresh", repo, why))
	return false, nil
}

// foreignFeed is why the bare repository gitDir is not one Fetch wrote, or "" when it is in the shape
// Fetch writes: a directory, not a gitfile, holding no symlink and nothing but regular files and
// directories, whose top level holds only what git's own bare init, clone and fetch write, whose
// configuration holds only the keys those and feedConfiguration write (feedConfigKeys), whose hooks
// are git's samples alone (none of which git runs), whose info holds only git's exclude, and whose
// objects name no alternates. Fetch runs git in this repository holding the provisioning token, and
// before this release an issue's agents could write the issue volume's feed directory, so a
// repository there in any other shape may carry a hook, configuration or link the fetch would run or
// follow. It reads the repository's files and runs nothing in it.
func foreignFeed(gitDir string) (string, error) {
	info, err := os.Lstat(gitDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return fmt.Sprintf("it is a %s, not a directory", fileKind(info.Mode())), nil
	}
	why := ""
	err = filepath.WalkDir(gitDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == gitDir {
			return nil
		}
		relative, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() && !entry.IsDir() {
			why = fmt.Sprintf("it holds %s, a %s", relative, fileKind(entry.Type()))
			return filepath.SkipAll
		}
		if refusal := feedEntryRefusal(filepath.ToSlash(relative), entry.IsDir()); refusal != "" {
			why = refusal
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil || why != "" {
		return why, err
	}
	config, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return "", err
	}
	return feedConfigRefusal(string(config)), nil
}

// feedEntryRefusal is why name, a path under a feed repository, is not one Fetch's git writes there,
// or "" when it is: the top level's HEAD, config, description, branches, hooks, info, objects, refs,
// packed-refs and FETCH_HEAD; under hooks only git's samples, which git never runs; under info only
// exclude and refs (the cache's repack runs update-server-info, which writes it, and no fetch reads
// it); under objects/info only what git writes of its own (no alternates, which would have the
// repository read another's objects); anything else under objects, and under refs and branches; and
// the temporary file update-server-info writes info/refs or objects/info/packs through, which a
// repack killed before its rename leaves and git never reads (serverInfoLeftover).
func feedEntryRefusal(name string, dir bool) string {
	if !dir && serverInfoLeftover.MatchString(name) {
		return ""
	}
	top, rest, nested := strings.Cut(name, "/")
	switch top {
	case "HEAD", "config", "description", "packed-refs", "FETCH_HEAD":
		if !nested && !dir {
			return ""
		}
	case "branches", "refs":
		return ""
	case "hooks":
		if !nested || (!dir && strings.HasSuffix(rest, ".sample") && !strings.Contains(rest, "/")) {
			return ""
		}
		return "it holds the hook " + name + ", which git would run"
	case "info":
		if !nested || ((rest == "exclude" || rest == "refs") && !dir) {
			return ""
		}
	case "objects":
		inner, ok := strings.CutPrefix(rest, "info/")
		if !ok || inner == "packs" || inner == "commit-graph" || inner == "commit-graphs" || strings.HasPrefix(inner, "commit-graphs/") {
			return ""
		}
	}
	return "it holds " + name + ", which no fetch writes"
}

// serverInfoLeftover is the temporary file git's update-server-info writes info/refs or
// objects/info/packs through, `<file>_` and six random characters (git's mks_tempfile), before it
// renames the file into place.
var serverInfoLeftover = regexp.MustCompile(`^(?:info/refs|objects/info/packs)_[A-Za-z0-9]{6}$`)

// feedConfigKeys are the configuration a feed repository may hold, each key (section.key, or
// section.subsection.key, with git's case rules: section and key lower-cased) with the values it may
// take: what git's bare init and clone write, and feedConfiguration. remote.origin.url is the one key
// a clone writes whose value is not fixed; Fetch names GitHub's URL in every command it runs and
// never the remote origin, so its value steers nothing.
var feedConfigKeys = map[string][]string{
	"core.repositoryformatversion": {"0", "1"},
	"core.filemode":                {"true", "false"},
	"core.bare":                    {"true"},
	"extensions.objectformat":      {"sha1"},
	"gc.auto":                      {"0"},
	"maintenance.auto":             {"false"},
	"fetch.unpacklimit":            {"1"},
	"remote.origin.url":            nil,
}

// feedConfigHeader and feedConfigPair are the only lines git writes in a configuration file: a
// section header alone on its line, `[section]` or `[section "subsection"]`, and `\tkey = value`
// with a value git reads as written (no quote, escape or comment). git also reads a key on its
// header's line (`[section]key=value`) and ends an unquoted value at `;` or `#`, so a line in any
// other form could set what no line of these two forms shows.
var (
	feedConfigHeader = regexp.MustCompile(`^\[([A-Za-z0-9-]+)(?: "([^"\\]*)")?\]$`)
	feedConfigPair   = regexp.MustCompile(`^\t([A-Za-z][A-Za-z0-9-]*) = ([^"\\;#]*)$`)
)

// feedConfigRefusal is why config, a feed repository's configuration file, is not one git and
// feedConfiguration wrote, or "" when it is: only whole lines of git's own two forms
// (feedConfigHeader, feedConfigPair), each key one feedConfigKeys allows with a value it allows.
// Anything else (a key on its header's line, an include, a comment, a quoted or escaped value, a key
// the list does not name) is refused, since git would read it and the fetch would act on it.
func feedConfigRefusal(config string) string {
	section := ""
	for number, line := range strings.Split(strings.TrimSuffix(config, "\n"), "\n") {
		if line == "" {
			continue
		}
		if header := feedConfigHeader.FindStringSubmatchIndex(line); header != nil {
			section = strings.ToLower(line[header[2]:header[3]])
			if header[4] >= 0 {
				section += "." + line[header[4]:header[5]]
			}
			continue
		}
		pair := feedConfigPair.FindStringSubmatch(line)
		if pair == nil || section == "" {
			return fmt.Sprintf("its configuration's line %d, %q, is not one git writes", number+1, line)
		}
		name, value := section+"."+strings.ToLower(pair[1]), pair[2]
		allowed, known := feedConfigKeys[name]
		if !known {
			return fmt.Sprintf("its configuration sets %s, which no fetch writes", name)
		}
		if allowed != nil && !slices.Contains(allowed, value) {
			return fmt.Sprintf("its configuration sets %s to %q, which no fetch writes", name, value)
		}
	}
	return ""
}

// fileKind names a file's kind as foreignFeed reports it.
func fileKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "directory"
	case mode.IsRegular():
		return "file"
	}
	return "special file"
}
