package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SkillsTreeDigest is the digest of a plugin's directory: sha256 over every regular file under
// root, each as its slash-separated path relative to root and its bytes, in the order a sorted walk
// visits them. It is the one definition of that digest, so the init container that records it and
// the check a role runs against what Oh My Pi installed compute the same number from the same tree
// (dispatch://INFRA-1300, R4).
//
// Every field is written length-prefixed — the decimal byte count, a newline, then exactly that
// many bytes — and a path holding a newline or a NUL is refused. Both are needed, and the second
// is what closes the hole review of pull request 2806 found: writing the fields merely separated,
// as `<path>\n<size>\n<content>` per file, the two-file tree {"a": "x", "b": "Y"} and the one-file
// tree {"a\n1\nxb": "Y"} write the same bytes, so a check over the one would pass the other. That
// collision needs a newline in a path and no other, since nothing else in the stream can stand in
// for the separator; refusing such a path therefore closes it, and the length prefixes make the
// stream unreadable two ways at all, because the digits end at the first newline and the count then
// fixes the field exactly. The prefixes are the invariant and the refusal is the fence, rather than
// either being asked to hold alone.
//
// Each file's executable bit is hashed with its bytes, because a skill's helper script that comes
// out not executable is a different thing from one that does. That is safe to hash because Oh My
// Pi's install of a local marketplace preserves the mode, which is measured rather than assumed: in
// a pod of the pinned image, a plugin tree holding a 0644 file and a 0755 file installed as 0644
// and 0755. Nothing finer than that bit is hashed: the rest of the mode is the umask's and the
// filesystem's business, not a statement about what the agent reads.
//
// A tree holding anything but a directory or a regular file is refused by name — a symlink above
// all, which would make the digest a statement about whatever it points at rather than about the
// tree, and which is how a plugin could carry something from outside the clone into what a role
// loads.
func SkillsTreeDigest(root string) (string, error) {
	sum := sha256.New()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return nil
		case !entry.Type().IsRegular():
			return fmt.Errorf("%s is %s, and a plugin tree may hold only directories and regular files", path, entry.Type())
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(relative)
		if strings.ContainsAny(slashed, "\n\x00") {
			return fmt.Errorf("%s holds a newline or a NUL in its name, which a plugin tree may not", path)
		}
		paths = append(paths, slashed)
		return nil
	}); err != nil {
		return "", fmt.Errorf("read the plugin tree %s: %w", root, err)
	}
	sort.Strings(paths)
	for _, relative := range paths {
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return "", fmt.Errorf("read the plugin tree %s: %w", root, err)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return "", fmt.Errorf("read the plugin tree %s: %w", root, err)
		}
		// Length-prefixed, so the stream parses one way: the path's byte count, the path, then
		// the content's byte count and the content. The size comes from the open file's own stat,
		// and a file that grew or shrank between the two is refused below, so the count and the
		// bytes can never disagree.
		fmt.Fprintf(sum, "%d\n%s%d\n%d\n", len(relative), relative, executableBit(info.Mode()), info.Size())
		written, err := io.Copy(sum, file)
		if err != nil {
			_ = file.Close()
			return "", fmt.Errorf("read the plugin tree %s: %w", root, err)
		}
		if written != info.Size() {
			_ = file.Close()
			return "", fmt.Errorf("%s was %d bytes and read as %d: a plugin tree must not change while it is digested", filepath.Join(root, filepath.FromSlash(relative)), info.Size(), written)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("read the plugin tree %s: %w", root, err)
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// ReadSkillsTrees is the TREES record as a plugin's digest by its name, in the order the record
// lists them, with that order returned beside the map: the check walks the plugins in the order
// the catalog named them, so its first failure is the first plugin that differs rather than
// whichever a map happened to yield.
func ReadSkillsTrees(dir string) (digests map[string]string, order []string, err error) {
	path := SkillsTreesFile(dir)
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read the skills record %s: %w", path, err)
	}
	digests = map[string]string{}
	for line := range strings.SplitSeq(strings.TrimRight(string(content), "\n"), "\n") {
		digest, plugin, found := strings.Cut(line, "  ")
		if !found || digest == "" || plugin == "" {
			return nil, nil, fmt.Errorf("the skills record %s holds the line %q, want `<sha256>  <plugin>`", path, line)
		}
		if _, held := digests[plugin]; held {
			return nil, nil, fmt.Errorf("the skills record %s names plugin %q twice", path, plugin)
		}
		digests[plugin] = digest
		order = append(order, plugin)
	}
	if len(order) == 0 {
		return nil, nil, fmt.Errorf("the skills record %s names no plugin", path)
	}
	return digests, order, nil
}

// CheckSkillsInstalled is what a role's start wrapper runs after its install: for every plugin the
// TREES record of the skills volume dir names, it digests the directory installed names for that
// plugin and compares it with the digest the init container recorded from the clone at the commit
// COMMIT names. A plugin the install left nowhere, one whose tree differs, and one the record does
// not name are each a refusal, naming the plugin and what differed — the role's launch then fails
// before Oh My Pi, rather than an agent running with skills nobody can account for
// (dispatch://INFRA-1300, R4).
//
// It is a comparison of bytes and not of a version string: a marketplace install that resolved a
// different plugin, a tree something wrote into the install cache after the install, and a volume
// whose clone changed under a retry all differ here, and none of them differs in a version.
// installed maps a plugin's name to the directory Oh My Pi reports it installed at; the caller
// reads that from Oh My Pi itself rather than from a path this package assumes.
func CheckSkillsInstalled(dir string, installed map[string]string) error {
	digests, order, err := ReadSkillsTrees(dir)
	if err != nil {
		return err
	}
	for plugin := range installed {
		if _, named := digests[plugin]; !named {
			return fmt.Errorf("plugin %q is installed but the skills record %s does not name it: the pod cannot say which commit its files came from",
				plugin, SkillsTreesFile(dir))
		}
	}
	for _, plugin := range order {
		root, ok := installed[plugin]
		if !ok || root == "" {
			return fmt.Errorf("plugin %q of the skills marketplace is not installed: Oh My Pi reports no directory for it", plugin)
		}
		digest, err := SkillsTreeDigest(root)
		if err != nil {
			return err
		}
		if digest != digests[plugin] {
			return fmt.Errorf("plugin %q installed at %s is %s, and the commit this pod cloned holds %s: what Oh My Pi would load is not what the pod's skills clone recorded",
				plugin, root, digest, digests[plugin])
		}
	}
	return nil
}

// skillsTrees digests each plugin's own directory of the clone, in the catalog's order: what
// CloneSkills records as TREES. pluginRoot is the catalog's `metadata.pluginRoot` and sources each
// plugin's `source` as the catalog spelled it, both already held inside the clone
// (skillsLocalSource), so this only has to resolve and digest them.
func skillsTrees(clone, pluginRoot string, plugins []string, sources map[string]string) (map[string]string, error) {
	digests := make(map[string]string, len(plugins))
	for _, plugin := range plugins {
		within := filepath.Join(filepath.FromSlash(pluginRoot), filepath.FromSlash(strings.TrimPrefix(sources[plugin], "./")))
		digest, err := SkillsTreeDigest(filepath.Join(clone, within))
		if err != nil {
			return nil, err
		}
		digests[plugin] = digest
	}
	return digests, nil
}

// writeSkillsTrees writes the TREES record, one `<sha256>  <plugin>` line per plugin in the
// catalog's order.
func writeSkillsTrees(dir string, plugins []string, digests map[string]string) error {
	var record strings.Builder
	for _, plugin := range plugins {
		fmt.Fprintf(&record, "%s  %s\n", digests[plugin], plugin)
	}
	path := SkillsTreesFile(dir)
	if err := os.WriteFile(path, []byte(record.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// executableBit is 1 when mode has any execute bit and 0 otherwise: the one part of a file's mode
// SkillsTreeDigest hashes.
func executableBit(mode fs.FileMode) int {
	if mode.Perm()&0o111 != 0 {
		return 1
	}
	return 0
}
