package ghconfig

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// Bundle is one role's gh directories: Default, the lowercased owner whose directory GH_CONFIG_DIR
// names (the issue's owner; the controller's first owner), and Owners, each owner's Rendered files,
// keyed by lowercased owner. Owners[Default] is the write token's; every other entry is read-only.
type Bundle struct {
	Default string
	Owners  map[string]Rendered
}

// ownerLogin is a lowercased GitHub login: letters, digits and hyphens, never starting with a
// hyphen. A login is a path segment under a bundle's root, so nothing else is one.
var ownerLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// IsOwner says whether owner is a lowercased GitHub login, and so a directory a bundle may hold.
func IsOwner(owner string) bool { return ownerLogin.MatchString(owner) }

// OwnerDir is the directory an owner's two gh files live in under a bundle root: <root>/<owner>.
func OwnerDir(root, owner string) string { return filepath.Join(root, owner) }

// OwnerNames is the bundle's owners, sorted.
func (b Bundle) OwnerNames() []string { return slices.Sorted(maps.Keys(b.Owners)) }

// Validate refuses a bundle with no Default, a Default absent from Owners, or an owner that is
// not a lowercased GitHub login (letters, digits, hyphens) — a login is a path segment here.
func (b Bundle) Validate() error {
	if b.Default == "" {
		return errors.New("gh bundle names no default owner")
	}
	for _, owner := range b.OwnerNames() {
		if !IsOwner(owner) {
			return fmt.Errorf("gh bundle owner %q is not a lowercased GitHub login", owner)
		}
	}
	if !IsOwner(b.Default) {
		return fmt.Errorf("gh bundle default owner %q is not a lowercased GitHub login", b.Default)
	}
	if _, ok := b.Owners[b.Default]; !ok {
		return fmt.Errorf("gh bundle default owner %q has no gh files in the bundle", b.Default)
	}
	return nil
}

// WriteBundle brings root to bundle: root made 0700, each owner's directory as Write brings one,
// and every other entry of root removed — an owner no longer in the bundle, the two files of a
// directory written before bundles, which held one role's files at its top, and anything else.
// root is the bundle's alone, and it records nothing of the bundle but the gh files: the pane's own
// user can write it, so a reader of the bundle's default or write owner reads the daemon's own
// record instead. It returns the owners whose hosts.yml changed, sorted, and writes nothing for a
// bundle Validate refuses.
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
	for _, owner := range bundle.OwnerNames() {
		ownerChanged, err := Write(OwnerDir(root, owner), bundle.Owners[owner])
		if err != nil {
			return nil, fmt.Errorf("write %s's gh files: %w", owner, err)
		}
		if ownerChanged {
			changed = append(changed, owner)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if _, ok := bundle.Owners[entry.Name()]; ok && entry.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return nil, fmt.Errorf("remove %s from the gh bundle: %w", entry.Name(), err)
		}
	}
	return changed, nil
}
