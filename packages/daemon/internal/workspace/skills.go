package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// The skills volume's layout, which both sides read: `legion workspace-init skills` writes it, and
// the Sandbox runtime renders the same paths into the init container's command and into each
// role's start wrapper. The clone is a directory of its own under the volume's root, so the four
// records sit beside it rather than inside a tree the clone's own `git checkout` owns.
//
// A role never writes any of it: the volume is read-only in the pod's one launcher container, and
// the container that fills it holds the provisioning token and mounts nothing else of the pod.
const (
	skillsCommitName      = "COMMIT"
	skillsMarketplaceName = "MARKETPLACE"
	skillsPluginsName     = "PLUGINS"
	skillsTreesName       = "TREES"
)

// skillsCloneName is the clone's directory under the skills volume root, fixed rather than the
// repository's own name: `ghrepo.Parse` admits a repository called `COMMIT`, `MARKETPLACE` or
// `PLUGINS`, and a clone named for one of those would be a directory at exactly the path the
// records are written to, so every pod of that deployment would fail its skills container on a
// name nobody would think to avoid. One fixed name also keeps the path a role's start wrapper
// names independent of the repository, so changing the repository changes no path.
const skillsCloneName = "repo"

// SkillsClone is where CloneSkills puts the clone under the skills volume root dir:
// `<dir>/repo`, the directory a role's `omp plugin marketplace add` names. The repository is taken
// for the signature's sake alone — the path does not depend on it (skillsCloneName) — so a caller
// cannot name the clone of one repository and the records of another. It is a plain join, so the
// Sandbox runtime can render the path into a role's start wrapper without having to handle a
// failure its configuration already ruled out; CloneSkills itself refuses the zero repository and
// an empty directory.
func SkillsClone(dir string, _ ghrepo.Repository) string {
	return filepath.Join(dir, skillsCloneName)
}

// SkillsCommitFile, SkillsMarketplaceFile and SkillsPluginsFile are the three records CloneSkills
// writes beside the clone, each a path under the skills volume root dir: the commit the clone is at
// (one line, the full object name), the marketplace catalog's own name (one line), and the plugins
// the catalog names (one per line, in the catalog's order). A person reads the commit to see which
// verification-skills a pod runs, and a role's start wrapper reads the other two to install from
// the clone without the daemon having to know the catalog's names at render time.
func SkillsCommitFile(dir string) string { return filepath.Join(dir, skillsCommitName) }

// SkillsMarketplaceFile is the record of the marketplace catalog's own `name`, one line: what a
// role's start wrapper names in the `omp plugin marketplace remove` that rebinds an earlier
// generation's registration to this pod's own directory, and as the marketplace half of each
// `<plugin>@<marketplace>` it installs and enables. The wrapper never runs `omp plugin marketplace
// update`: that fetches the registry record's own source, which an earlier generation may have
// added as `owner/repo`.
func SkillsMarketplaceFile(dir string) string { return filepath.Join(dir, skillsMarketplaceName) }

// SkillsPluginsFile is the record of the plugins the catalog names, one per line and in the
// catalog's order: what a role's start wrapper reads to install each of them from the clone.
func SkillsPluginsFile(dir string) string { return filepath.Join(dir, skillsPluginsName) }

// SkillsTreesFile is the record of what each plugin's directory held at the commit the clone was
// made at, one `<sha256>  <plugin>` line per plugin in the catalog's order: what
// `legion skills-check` compares a role's installed plugin against, so a pod proves that what Oh
// My Pi loaded is the bytes of the commit COMMIT names and not something that reached the volume
// afterwards. It is written by the same process and from the same clone that COMMIT records, so
// the digest and the commit are one statement rather than two that might disagree.
func SkillsTreesFile(dir string) string { return filepath.Join(dir, skillsTreesName) }

// SkillsRequest is the skills init container's: the repository to clone, how to read the
// provisioning token (Token, read once — the clone is one transfer, not Fetch's several), the
// directory its one-shot credential goes under, the skills volume's root, and the log each step is
// told.
type SkillsRequest struct {
	Repo          ghrepo.Repository
	Token         func() (string, error)
	CredentialDir string
	Dir           string
	Log           func(line string)
}

// SkillsReport is what CloneSkills cloned: the commit, the branch that commit is the tip of, the
// marketplace catalog's name, and the plugins it names.
type SkillsReport struct {
	Commit string
	// Branch is the repository's default branch as GitHub names it, which is the branch the clone
	// took: `git clone --single-branch` with no `--branch` follows GitHub's own HEAD. Reported so
	// a person can read which branch a pod's skills came from; nothing checks it against a name,
	// since Legion holds no opinion about what a repository calls its default branch.
	Branch      string
	Marketplace string
	Plugins     []string
	// Trees are each plugin's tree digest, by plugin name, as SkillsTreeDigest computes it over
	// that plugin's directory of this clone: what the TREES record holds and what
	// `legion skills-check` compares a role's installed plugin against (dispatch://INFRA-1300, R4).
	Trees map[string]string
}

// skillsCatalogFiles are the catalog paths inside a marketplace repository, in the order Oh My Pi
// reads them (its marketplace source resolver): its own first, then the Claude-plugin spelling
// every theorem-labs skills repository uses today.
var skillsCatalogFiles = []string{".omp-plugin/marketplace.json", ".claude-plugin/marketplace.json"}

// CloneSkills clones repository shallow into the skills volume and writes the three records beside
// it (SkillsCommitFile and its siblings). It is the one process of a pod that holds the
// provisioning token besides Fetch, and it runs in a container that mounts nothing an agent of the
// issue can write, so the clone a role later reads is never something a role wrote.
//
// It is a plain `git clone --depth 1 --single-branch` of the repository's default branch as GitHub
// names it, under the same one-shot credential and the same pinned, isolated git configuration
// Fetch's transfers run under: a helper for https://github.com alone, no askpass, no terminal
// prompt, and neither the system's nor the image user's configuration. The credential is removed
// as soon as the transfer ends, before reading the clone; a removal failure is returned alongside
// any transfer failure rather than hidden behind it.
//
// An earlier attempt's clone, records and one-shot credentials are removed first: the kubelet
// restarts a failed init container on the same skills and TMPDIR volumes, so a killed attempt can
// leave both a partial tree and a live token. Neither may survive a retry that itself fails before
// cloning. The shallow, single-branch clone needs the files of one commit, never the history.
func CloneSkills(ctx context.Context, run Runner, request SkillsRequest) (report SkillsReport, err error) {
	switch {
	case request.Repo.IsZero():
		return SkillsReport{}, errors.New("skills repository is required")
	case request.Dir == "":
		return SkillsReport{}, errors.New("skills directory is required")
	case request.Log == nil:
		return SkillsReport{}, errors.New("skills request names no log")
	}
	clone := SkillsClone(request.Dir, request.Repo)
	// The token the credential is actually built with, captured as the source reads it: git echoes
	// the remote's URL, and a failed clone's stderr reaches the container log (`workspace-init
	// skills`), so what this returns on a failure must carry no token — the same rule Fetch keeps
	// for its own transfers (fetchCredential.scrub). Reading request.Token a second time would
	// scrub the wrong value, since the daemon re-mints the lease and the file the pointer names is
	// rewritten under a running container.
	presented := ""
	source := gitHubSource{credentialDir: request.CredentialDir}
	if request.Token != nil {
		source.token = func() (string, error) {
			token, err := request.Token()
			presented = token
			return token, err
		}
	}
	if err := source.check(request.Repo); err != nil {
		return SkillsReport{}, err
	}
	// This TMPDIR belongs to skills-fetch alone. Never leave a killed attempt's token behind
	// merely because opening the next credential (or preparing the clone) fails.
	entries, readErr := os.ReadDir(request.CredentialDir)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return SkillsReport{}, fmt.Errorf("read the skills credential directory: %w", readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "provisioning-credential-") {
			if err := os.RemoveAll(filepath.Join(request.CredentialDir, entry.Name())); err != nil {
				return SkillsReport{}, fmt.Errorf("remove an earlier skills provisioning credential: %w", err)
			}
		}
	}
	if err := os.MkdirAll(request.Dir, 0o700); err != nil {
		return SkillsReport{}, fmt.Errorf("create the skills directory %s: %w", request.Dir, err)
	}
	for _, record := range []string{SkillsCommitFile(request.Dir), SkillsMarketplaceFile(request.Dir), SkillsPluginsFile(request.Dir), SkillsTreesFile(request.Dir)} {
		if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
			return SkillsReport{}, fmt.Errorf("remove the skills record %s an earlier attempt left: %w", record, err)
		}
	}
	if err := os.RemoveAll(clone); err != nil {
		return SkillsReport{}, fmt.Errorf("remove the skills clone %s an earlier attempt left: %w", clone, err)
	}
	credential, err := source.open(request.Repo, "", "")
	if err != nil {
		return SkillsReport{}, err
	}
	defer func() {
		err = errors.Join(err, credential.remove())
	}()
	env := merge(credential.env, isolatedGitConfig)
	argv := []string{"git", "clone", "--depth", "1", "--single-branch", "--quiet", GitHubURL(request.Repo), clone}
	request.Log(fmt.Sprintf("cloning %s into %s", request.Repo, clone))
	if _, err := RunChecked(ctx, run, argv, env, request.Dir); err != nil {
		return SkillsReport{}, errors.New(scrubToken(err.Error(), presented))
	}
	if err := credential.remove(); err != nil {
		return SkillsReport{}, err
	}
	report, err = skillsRecord(ctx, run, clone)
	if err != nil {
		return SkillsReport{}, err
	}
	if err = writeSkillsRecords(request.Dir, report); err != nil {
		return SkillsReport{}, err
	}
	return report, nil
}

// skillsRecord reads what the clone is: its commit (`git rev-parse HEAD`, run with no credential
// and no configuration but the clone's own) and the marketplace catalog it carries. A clone with
// no catalog, one whose catalog does not parse, one that names no marketplace name and one that
// names no plugin are each the container's failure: a pod whose skills volume holds a tree no role
// can install from is a pod whose roles would start without their skills, which is the failure this
// container exists to prevent.
func skillsRecord(ctx context.Context, run Runner, clone string) (SkillsReport, error) {
	result, err := RunChecked(ctx, run, []string{"git", "-C", clone, "rev-parse", "HEAD"}, isolatedGitConfig, clone)
	if err != nil {
		return SkillsReport{}, err
	}
	commit := strings.TrimSpace(result.Stdout)
	if commit == "" {
		return SkillsReport{}, fmt.Errorf("the skills clone %s names no commit", clone)
	}
	// The branch the clone is on: the repository's default branch as GitHub names it, since the
	// clone names no branch of its own. It is reported, not checked, so a person reading the
	// container's last line sees which branch a pod's skills came from rather than having to trust
	// that the repository's default is still the branch they meant.
	branch, err := RunChecked(ctx, run, []string{"git", "-C", clone, "rev-parse", "--abbrev-ref", "HEAD"}, isolatedGitConfig, clone)
	if err != nil {
		return SkillsReport{}, err
	}
	catalog, pluginRoot, sources, err := readSkillsCatalog(clone)
	if err != nil {
		return SkillsReport{}, err
	}
	// The digest of each plugin's tree, taken from this clone, which is the commit above: what a
	// role's `legion skills-check` compares its installed plugin against (dispatch://INFRA-1300,
	// R4). It is taken here, in the container that made the clone and holds the token, and never
	// in the role's launch, so the number a role checks against is one no role could have written.
	if catalog.Trees, err = skillsTrees(clone, pluginRoot, catalog.Plugins, sources); err != nil {
		return SkillsReport{}, err
	}
	catalog.Commit = commit
	catalog.Branch = strings.TrimSpace(branch.Stdout)
	return catalog, nil
}

// readSkillsCatalog is the marketplace catalog of the clone: the first of skillsCatalogFiles it
// holds, its `name`, and the `name` and `source` of each of its `plugins`. A name with a newline,
// a space or a shell metacharacter in it is refused: the role's start wrapper reads these two
// files into shell words, and one line per name is what that reading takes.
//
// A plugin whose `source` is anything but a relative path inside this clone is refused too, and
// that is the refusal this whole arrangement rests on. Oh My Pi's catalog admits a plugin source
// that is a github repository, a git or http URL, or an npm package, and `omp plugin install`
// fetches such a source itself — in the role's launch context, which is where a fetch ends the
// pod's gVisor sandbox about four seconds in (dispatch://INFRA-1262). A local marketplace whose
// catalog named one would therefore reintroduce exactly the crash the clone exists to avoid, so a
// catalog that names one fails this container instead, by the variant it declares.
func readSkillsCatalog(clone string) (report SkillsReport, pluginRoot string, sources map[string]string, err error) {
	var tried []string
	for _, name := range skillsCatalogFiles {
		path := filepath.Join(clone, filepath.FromSlash(name))
		tried = append(tried, name)
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return SkillsReport{}, "", nil, fmt.Errorf("read the marketplace catalog %s: %w", path, err)
		}
		// Every object this reads is checked for a repeated key and for a key that differs from one
		// it reads only by case, before any of it is decoded: Go's decoder and Oh My Pi's
		// JavaScript one resolve such a catalog differently, and the difference would be a plugin
		// the installer fetches and this check never saw (checkSkillsObjectKeys).
		if err := checkSkillsObjectKeys(content, catalogKeys, "the marketplace catalog", path); err != nil {
			return SkillsReport{}, "", nil, err
		}
		var catalog struct {
			Name     string            `json:"name"`
			Owner    json.RawMessage   `json:"owner"`
			Metadata json.RawMessage   `json:"metadata"`
			Plugins  []json.RawMessage `json:"plugins"`
		}
		if err := json.Unmarshal(content, &catalog); err != nil {
			return SkillsReport{}, "", nil, fmt.Errorf("parse the marketplace catalog %s: %w", path, err)
		}
		if err := skillsName("the marketplace catalog's name", catalog.Name, path); err != nil {
			return SkillsReport{}, "", nil, err
		}
		if npmDistTags[strings.ToLower(catalog.Name)] {
			return SkillsReport{}, "", nil, fmt.Errorf("the marketplace catalog's name in %s is %q: Oh My Pi reads that word after an \"@\" as an npm dist-tag rather than a marketplace, so the install would fetch from npm", path, catalog.Name)
		}
		if err := checkSkillsOwner(catalog.Owner, path); err != nil {
			return SkillsReport{}, "", nil, err
		}
		root, err := readSkillsPluginRoot(catalog.Metadata, path)
		if err != nil {
			return SkillsReport{}, "", nil, err
		}
		read := SkillsReport{Marketplace: catalog.Name}
		held := map[string]string{}
		for index, raw := range catalog.Plugins {
			named := fmt.Sprintf("plugins[%d] of the marketplace catalog", index)
			if err := checkSkillsObjectKeys(raw, pluginKeys, named, path); err != nil {
				return SkillsReport{}, "", nil, err
			}
			var plugin struct {
				Name   string          `json:"name"`
				Source json.RawMessage `json:"source"`
			}
			if err := json.Unmarshal(raw, &plugin); err != nil {
				return SkillsReport{}, "", nil, fmt.Errorf("parse %s in %s: %w", named, path, err)
			}
			if err := skillsName("a plugin name", plugin.Name, path); err != nil {
				return SkillsReport{}, "", nil, err
			}
			source, err := skillsLocalSource(clone, root, plugin.Name, plugin.Source, path)
			if err != nil {
				return SkillsReport{}, "", nil, err
			}
			if _, twice := held[plugin.Name]; twice {
				return SkillsReport{}, "", nil, fmt.Errorf("the marketplace catalog %s names plugin %q twice", path, plugin.Name)
			}
			held[plugin.Name] = source
			read.Plugins = append(read.Plugins, plugin.Name)
		}
		if len(read.Plugins) == 0 {
			return SkillsReport{}, "", nil, fmt.Errorf("the marketplace catalog %s names no plugin", path)
		}
		return read, root, held, nil
	}
	return SkillsReport{}, "", nil, fmt.Errorf("the skills clone %s holds no marketplace catalog (%s)", clone, strings.Join(tried, " or "))
}

// skillsLocalSource refuses a plugin whose source is not a directory of this clone: anything but a
// JSON string beginning `./`, a path that leaves the clone (`..`, an absolute path, a path through
// a symlink pointing out of it), and a path the clone does not hold. An object source is refused by
// the variant it declares (`github`, `url`, `git-subdir`, `npm`), since each of those is a source
// `omp plugin install` fetches itself. The path is resolved with its symlinks followed, so a
// `./plugins/x` that is a link to `/etc` is refused rather than installed from.
//
// pluginRoot is the catalog's own `metadata.pluginRoot`, which Oh My Pi joins in front of a
// relative source before it resolves it, so a catalog that sets it and a check that ignored it
// would disagree about which directory is installed — the check would pass a path outside the
// clone, or refuse a catalog whose plugins are perfectly in it. It is joined here for that reason,
// and the result is held inside the clone whatever either half says.
func skillsLocalSource(clone, pluginRoot, plugin string, source json.RawMessage, path string) (string, error) {
	if len(source) == 0 {
		return "", fmt.Errorf("plugin %q in %s names no source: a plugin of a local marketplace must be a directory of the clone, as \"./plugins/%s\"", plugin, path, plugin)
	}
	var relative string
	if err := json.Unmarshal(source, &relative); err != nil {
		named := "an object"
		var object struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(source, &object) == nil && object.Source != "" {
			named = fmt.Sprintf("the %q source", object.Source)
		}
		return "", fmt.Errorf("plugin %q in %s names %s, which `omp plugin install` would fetch at a role's launch: a plugin of a local marketplace must be a directory of the clone, as \"./plugins/%s\"",
			plugin, path, named, plugin)
	}
	if !strings.HasPrefix(relative, "./") {
		return "", fmt.Errorf("plugin %q in %s names the source %q, which `omp plugin install` would fetch at a role's launch: a plugin of a local marketplace must be a relative path beginning \"./\"",
			plugin, path, relative)
	}
	within := filepath.Join(filepath.FromSlash(pluginRoot), filepath.FromSlash(strings.TrimPrefix(relative, "./")))
	resolved, err := filepath.EvalSymlinks(filepath.Join(clone, within))
	if err != nil {
		return "", fmt.Errorf("plugin %q in %s names the source %q under the plugin root %q, which the clone does not hold: %w", plugin, path, relative, pluginRoot, err)
	}
	root, err := filepath.EvalSymlinks(clone)
	if err != nil {
		return "", fmt.Errorf("resolve the skills clone %s: %w", clone, err)
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("plugin %q in %s names the source %q under the plugin root %q, which resolves to %s, outside the clone %s",
			plugin, path, relative, pluginRoot, resolved, root)
	}
	return relative, nil
}

// skillsNameAllowed is what a marketplace or plugin name may hold: ASCII letters, digits, hyphens,
// underscores and dots, and a first character that is a letter. Nothing in that set is a shell word
// break or metacharacter, so the start wrapper's `"$name"` reading of these files carries each name
// whole.
//
// The first-letter rule is narrower than what Oh My Pi admits as a marketplace name, and it is
// narrower on purpose: a role installs a plugin as `<plugin>@<marketplace>`, and Oh My Pi's install
// reads that word as a package at a version rather than a plugin of a marketplace when the half
// after the `@` looks like a version — `latest`, or anything beginning with a digit — which would
// send the install to npm in the role's launch context, the one place a fetch must never happen
// (dispatch://INFRA-1262). A name this refuses is a name a person renames in the marketplace
// repository, which costs a commit; a name that reaches npm costs the pod.
func skillsNameAllowed(name string) bool {
	if name == "" || !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return name != ""
}

// npmDistTags are the words Oh My Pi reads as an npm dist-tag after an `@`, rather than as a
// marketplace, when it classifies what `omp plugin install <plugin>@<name>` names
// (`classify-install-target.ts`, read as source at omp 18.16; the pinned 18.13 was not measured, so
// refusing these is precaution rather than a failure reproduced here). A marketplace whose catalog
// named one of them would send a role's install to npm — a fetch in the role's launch context,
// which is the one place a fetch must never happen (dispatch://INFRA-1262) — so a catalog that
// names one fails the skills container instead, where a person can rename it. A plugin name may be
// any of them: it is the half before the `@`, which is never read as a version.
var npmDistTags = map[string]bool{
	"latest": true, "next": true, "beta": true, "alpha": true, "canary": true,
	"rc": true, "dev": true, "stable": true, "nightly": true, "experimental": true,
}

func skillsName(what, name, path string) error {
	if !skillsNameAllowed(name) {
		return fmt.Errorf("%s in %s is %q: a name beginning with an ASCII letter and holding only ASCII letters, digits, hyphens, underscores and dots is required", what, path, name)
	}
	return nil
}

// scrubToken is text with token replaced by ***, and text itself when there is no token to take
// out: what a failed transfer returns and the container log then carries must hold no credential,
// which is the rule Fetch keeps for its own transfers (fetchCredential.scrub).
func scrubToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "***")
}

// writeSkillsRecords writes the four records beside the clone, each world-readable within the pod
// (the launcher container reads them read-only) and holding nothing secret: the commit, the
// marketplace name, and one plugin name per line.
func writeSkillsRecords(dir string, report SkillsReport) error {
	for _, record := range []struct {
		path, content string
	}{
		{SkillsCommitFile(dir), report.Commit + "\n"},
		{SkillsMarketplaceFile(dir), report.Marketplace + "\n"},
		{SkillsPluginsFile(dir), strings.Join(report.Plugins, "\n") + "\n"},
	} {
		if err := os.WriteFile(record.path, []byte(record.content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", record.path, err)
		}
	}
	return writeSkillsTrees(dir, report.Plugins, report.Trees)
}
