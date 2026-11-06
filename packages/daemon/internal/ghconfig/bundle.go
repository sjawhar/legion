package ghconfig

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Bundle is one role's gh directories: Default, the lowercased owner whose directory GH_CONFIG_DIR
// names (the issue's owner; the controller's first owner), and Dirs, each directory's Rendered
// files keyed by the directory's name under the bundle root. A name is a lowercased owner login,
// whose token reaches what the role may do across that owner, or `<owner>/<repo>`, both
// lowercased, a directory under the owner's own whose token reaches that one repository and no
// other. Dirs[Default] is the write token's where the role writes to one; every other entry is
// read-only. The name authorises nothing — the token in the directory does — so a reader that
// wants the narrowest token for a repository reads `<owner>/<repo>` where the bundle holds it.
type Bundle struct {
	Default string
	Dirs    map[string]Rendered
}

// ownerLogin is a lowercased GitHub login: letters, digits and hyphens, never starting with a
// hyphen. A login is a path segment under a bundle's root, so nothing else is one.
var ownerLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// repositoryName is a lowercased GitHub repository name: at most 100 letters, digits, `-`, `_`
// and `.`, which is GitHub's own grammar (ghrepo.Parse) lowercased. A name is a path segment
// under an owner's directory, and `.` and `..` are refused beside it, since either would name
// another directory than the repository's.
var repositoryName = regexp.MustCompile(`^[a-z0-9._-]{1,100}$`)

// IsOwner says whether owner is a lowercased GitHub login, and so a directory a bundle may hold.
// It is the rule for a caller that means an owner and nothing narrower: the bundle's Default,
// which GH_CONFIG_DIR names, and the owner a `gh` or a git-credential request names.
func IsOwner(owner string) bool { return ownerLogin.MatchString(owner) }

// IsDirName says whether name is a directory a bundle may hold: a lowercased owner login
// (IsOwner), or that login and one of its repositories as `<owner>/<repo>`, both lowercased. A
// GitHub login holds no slash and a repository name holds none either, so the one slash of a
// name splits it exactly.
//
// HostsFile and ConfigFile are no repository directory, though both are repository names GitHub
// allows: an owner's own two gh files sit beside its repositories' directories, so a bundle
// holding `<owner>/hosts.yml` would have WriteBundle make a directory where that owner's
// hosts.yml is and fail every directory of the bundle. A repository of either name is left out
// of the bundle instead (appauth's bundleDirs).
func IsDirName(name string) bool {
	owner, repo, nested := strings.Cut(name, "/")
	if !IsOwner(owner) {
		return false
	}
	if !nested {
		return true
	}
	return repo != "." && repo != ".." && repo != HostsFile && repo != ConfigFile && repositoryName.MatchString(repo)
}

// Dir is the directory a bundle entry's two gh files live in under a bundle root: <root>/<name>,
// which for a `<owner>/<repo>` entry is a directory under the owner's own.
func Dir(root, name string) string { return filepath.Join(root, name) }

// DirNames is the bundle's directory names, sorted. An owner sorts before its own repositories'
// directories, since each of those begins with the owner's name.
func (b Bundle) DirNames() []string { return slices.Sorted(maps.Keys(b.Dirs)) }

// Validate refuses a bundle with no Default, a Default absent from Dirs or that is not a
// lowercased GitHub login — GH_CONFIG_DIR always names an owner's directory, never a
// repository's — or a directory that is neither shape a bundle holds (IsDirName), each name being a
// path under the bundle's root.
func (b Bundle) Validate() error {
	if b.Default == "" {
		return errors.New("gh bundle names no default owner")
	}
	for _, name := range b.DirNames() {
		if !IsDirName(name) {
			return fmt.Errorf("gh bundle directory %q is no lowercased GitHub login or owner/repository", name)
		}
	}
	if !IsOwner(b.Default) {
		return fmt.Errorf("gh bundle default owner %q is not a lowercased GitHub login", b.Default)
	}
	if _, ok := b.Dirs[b.Default]; !ok {
		return fmt.Errorf("gh bundle default owner %q has no gh files in the bundle", b.Default)
	}
	return nil
}

// WriteBundle brings root to bundle: root made 0700, each directory as Write brings one, and both
// levels cleaned of what the bundle no longer holds. An entry of the root that is no owner of the
// bundle goes — an owner no longer served, the two files of a directory written before bundles,
// which held one role's files at its top, and anything else — and under each owner's directory a
// subdirectory that is no repository entry of the bundle goes too, so a repository the mapping no
// longer serves is left holding no stale token; the owner's own hosts.yml and config.yml stay.
// root is the bundle's alone, and it records nothing of the bundle but the gh files: the pane's own
// user can write it, so a reader of the bundle's default or write owner reads the daemon's own
// record instead. It returns the directories whose hosts.yml changed, sorted, and writes nothing
// for a bundle Validate refuses.
func WriteBundle(root string, bundle Bundle) (changed []string, err error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	// Sorted, so an owner's own directory is written before any of its repositories' are made
	// under it.
	for _, name := range bundle.DirNames() {
		dirChanged, err := Write(Dir(root, name), bundle.Dirs[name])
		if err != nil {
			return nil, fmt.Errorf("write %s's gh files: %w", name, err)
		}
		if dirChanged {
			changed = append(changed, name)
		}
	}
	// held is each owner of the bundle by the repository directories it holds, which is the whole
	// of what either level of the root may keep.
	held := map[string]map[string]bool{}
	for name := range bundle.Dirs {
		owner, repo, nested := strings.Cut(name, "/")
		repos, ok := held[owner]
		if !ok {
			repos = map[string]bool{}
			held[owner] = repos
		}
		if nested {
			repos[repo] = true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		repos, owned := held[entry.Name()]
		if !owned || !entry.IsDir() {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return nil, fmt.Errorf("remove %s from the gh bundle: %w", entry.Name(), err)
			}
			continue
		}
		ownerDir := Dir(root, entry.Name())
		under, err := os.ReadDir(ownerDir)
		if err != nil {
			return nil, err
		}
		for _, sub := range under {
			if !sub.IsDir() || repos[sub.Name()] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(ownerDir, sub.Name())); err != nil {
				return nil, fmt.Errorf("remove %s/%s from the gh bundle: %w", entry.Name(), sub.Name(), err)
			}
		}
	}
	return changed, nil
}
