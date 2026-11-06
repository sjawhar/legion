package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// catalogAt writes body as the clone's Claude-plugin catalog and makes the plugin directories a
// valid catalog of it would name, so a case under test fails for its keys and not for a missing
// tree.
func catalogAt(t *testing.T, body string) string {
	t.Helper()
	clone := t.TempDir()
	for _, plugin := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(clone, "plugins", plugin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(clone, "plugins", plugin, "skill.md"), []byte("A skill.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(clone, ".claude-plugin", "marketplace.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return clone
}

// A catalog whose keys mean one thing to Go's reader and another to Oh My Pi's is refused rather
// than guessed at. The first case is the one that matters most: Go matches `Source`
// case-insensitively and the later key wins, so the catalog validates as a local path, while Oh My
// Pi destructures `source` and clones the github repository — a fetch in the role's launch, before
// anything checks what was installed.
func TestReadSkillsCatalogRefusesKeysTheTwoReadersResolveDifferently(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"a plugin source shadowed by its case variant": {
			`{"name":"skills","owner":{"name":"acme"},"plugins":[{"name":"one","source":{"source":"github","repo":"acme/remote"},"Source":"./plugins/one"}]}`,
			`names the key "Source", which differs from "source" only by case`,
		},
		"a plugin source given only by a case variant": {
			`{"name":"skills","owner":{"name":"acme"},"plugins":[{"name":"one","Source":"./plugins/one"}]}`,
			`names the key "Source", which differs from "source" only by case`,
		},
		"a plugin source repeated": {
			`{"name":"skills","owner":{"name":"acme"},"plugins":[{"name":"one","source":"./plugins/one","source":{"source":"github","repo":"acme/remote"}}]}`,
			`names the key "source" twice`,
		},
		"a plugin name shadowed by its case variant": {
			`{"name":"skills","owner":{"name":"acme"},"plugins":[{"Name":"one","source":"./plugins/one"}]}`,
			`names the key "Name", which differs from "name" only by case`,
		},
		"the catalog's name shadowed by its case variant": {
			`{"name":"skills","Name":"latest","owner":{"name":"acme"},"plugins":[{"name":"one","source":"./plugins/one"}]}`,
			`names the key "Name", which differs from "name" only by case`,
		},
		"the catalog's name repeated": {
			`{"name":"skills","name":"other","owner":{"name":"acme"},"plugins":[{"name":"one","source":"./plugins/one"}]}`,
			`names the key "name" twice`,
		},
		"the plugin root shadowed by its case variant": {
			`{"name":"skills","owner":{"name":"acme"},"metadata":{"pluginRoot":"plugins","PluginRoot":".."},"plugins":[{"name":"one","source":"./one"}]}`,
			`names the key "PluginRoot", which differs from "pluginRoot" only by case`,
		},
		"the plugin root repeated": {
			`{"name":"skills","owner":{"name":"acme"},"metadata":{"pluginRoot":"plugins","pluginRoot":".."},"plugins":[{"name":"one","source":"./one"}]}`,
			`names the key "pluginRoot" twice`,
		},
		"the plugins key shadowed by its case variant": {
			`{"name":"skills","owner":{"name":"acme"},"Plugins":[],"plugins":[{"name":"one","source":"./plugins/one"}]}`,
			`names the key "Plugins", which differs from "plugins" only by case`,
		},
		"the owner's name shadowed by its case variant": {
			`{"name":"skills","owner":{"Name":"acme"},"plugins":[{"name":"one","source":"./plugins/one"}]}`,
			`names the key "Name", which differs from "name" only by case`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := readSkillsCatalog(catalogAt(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readSkillsCatalog = %v, want a refusal naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "cannot be read for certain") {
				t.Errorf("the refusal %v does not say why the catalog cannot be read", err)
			}
		})
	}
}

// Oh My Pi's own reader of the catalog requires an owner with a name and refuses a catalog without
// one, so a clone missing it would fail a role's install rather than this container. It is read
// here instead, where the refusal names the file and the field.
func TestReadSkillsCatalogRefusesACatalogWithNoOwner(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no owner at all": {
			`{"name":"skills","plugins":[{"name":"one","source":"./plugins/one"}]}`,
			"names no owner",
		},
		"an owner with no name": {
			`{"name":"skills","owner":{"email":"nobody@example.invalid"},"plugins":[{"name":"one","source":"./plugins/one"}]}`,
			"has no name",
		},
		"an owner with an empty name": {
			`{"name":"skills","owner":{"name":""},"plugins":[{"name":"one","source":"./plugins/one"}]}`,
			"has no name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := readSkillsCatalog(catalogAt(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readSkillsCatalog = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// A catalog carries fields neither Legion nor this check reads, and those are its own business: a
// key that is not a case variant of a key this reads passes, whatever it is called.
func TestReadSkillsCatalogTakesTheKeysItDoesNotRead(t *testing.T) {
	body := `{"name":"skills","owner":{"name":"acme","url":"https://example.invalid"},` +
		`"metadata":{"pluginRoot":"plugins","description":"a marketplace"},` +
		`"allowCrossMarketplaceDependenciesOn":["acme"],` +
		`"plugins":[{"name":"one","source":"./one","version":"1.0.0","keywords":["rocq"],"strict":true}]}`
	report, root, sources, err := readSkillsCatalog(catalogAt(t, body))
	if err != nil {
		t.Fatalf("readSkillsCatalog = %v, want it to take a catalog whose extra keys it does not read", err)
	}
	if report.Marketplace != "skills" || root != "plugins" || sources["one"] != "./one" {
		t.Errorf("read marketplace %q, plugin root %q, sources %v", report.Marketplace, root, sources)
	}
}
