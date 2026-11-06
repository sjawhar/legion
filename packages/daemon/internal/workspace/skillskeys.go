package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// skillsObjectKeys are a JSON object's keys in the order they are written, duplicates included:
// what `json.Unmarshal` into a struct or a map cannot show, because both keep one value per key
// and neither says which spelling it came from.
//
// It exists for one reason. Go's decoder matches a struct field to a key case-insensitively when no
// key matches exactly, and whichever matching key comes last in the document wins; Oh My Pi reads
// the same catalog with JavaScript's own parser, which destructures `source` and nothing else. So a
// catalog carrying both `source` and `Source` is read as two different things by the two readers: a
// check that admitted the local `"./one"` would let the installer clone the `github` one it never
// saw — and that clone runs in the role's launch, before any check of what was installed
// (dispatch://INFRA-1300, found by review of pull request 2806). The same holds for a repeated key,
// which JavaScript resolves to the last and Go to the last as well, but which no reader of the file
// can see. A catalog whose keys cannot mean one thing to both readers is refused rather than
// guessed at.
//
// A value that is not an object yields no keys and no error: the caller checks its shape where it
// needs to.
func skillsObjectKeys(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, nil
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("a JSON object key is %v, not a string", token)
		}
		keys = append(keys, key)
		// Skip the value whole, whatever its shape, so the next token is the next key.
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// checkSkillsObjectKeys refuses an object of the marketplace catalog whose keys cannot mean one
// thing to both readers of that file (skillsObjectKeys): a key repeated, and a key that differs
// from one of known only by case. what names the object in the refusal, and path the file.
//
// Only a case-variant of a *known* key is refused, never an unknown key of its own: a marketplace
// catalog carries fields neither Legion nor this check reads (`keywords`, `category`, an author's
// own), and refusing those would make every pod of a deployment fail on a field somebody added for
// a reason of their own. A variant of a key this check reads is the dangerous one, because that is
// the key the two readers can disagree about.
func checkSkillsObjectKeys(raw json.RawMessage, known []string, what, path string) error {
	keys, err := skillsObjectKeys(raw)
	if err != nil {
		// The key walk is the first thing that reads the file, so malformed JSON surfaces here
		// rather than at the decode below; it is a parse failure and says so.
		return fmt.Errorf("parse %s in %s: %w", what, path, err)
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			return fmt.Errorf("%s in %s names the key %q twice: a repeated key means one thing to Go's reader of this file and another to Oh My Pi's, so the catalog cannot be read for certain",
				what, path, key)
		}
		seen[key] = true
		for _, name := range known {
			if key != name && strings.EqualFold(key, name) {
				return fmt.Errorf("%s in %s names the key %q, which differs from %q only by case: Oh My Pi reads %q and this check could read %q, so the catalog cannot be read for certain",
					what, path, key, name, name, key)
			}
		}
	}
	return nil
}

// catalogKeys and pluginKeys are the keys of the marketplace catalog, and of each of its plugins,
// that this package or Oh My Pi's own reader of the same file reads. A key of the file that
// differs from one of these only by case is refused (checkSkillsObjectKeys); any other key the
// file carries is its own business.
var (
	catalogKeys  = []string{"name", "owner", "metadata", "plugins"}
	pluginKeys   = []string{"name", "source", "version", "description"}
	metadataKeys = []string{"pluginRoot", "description", "version"}
)

// checkSkillsOwner refuses a catalog that names no owner, or an owner with no name: Oh My Pi's own
// catalog reader requires both and refuses the catalog without them, so a clone that reached a
// role's launch with either missing would fail that role's install rather than this container,
// where the failure names the file and the field.
func checkSkillsOwner(raw json.RawMessage, path string) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("the marketplace catalog %s names no owner: Oh My Pi's own reader of it requires an owner with a name", path)
	}
	if err := checkSkillsObjectKeys(raw, []string{"name", "email", "url"}, "the marketplace catalog's owner", path); err != nil {
		return err
	}
	var owner struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(raw, &owner); err != nil {
		return fmt.Errorf("parse the marketplace catalog's owner in %s: %w", path, err)
	}
	if owner.Name == nil || *owner.Name == "" {
		return fmt.Errorf("the marketplace catalog's owner in %s has no name: Oh My Pi's own reader of it requires one", path)
	}
	return nil
}

// readSkillsPluginRoot is the catalog's `metadata.pluginRoot`, "" when it sets none: the directory
// Oh My Pi joins in front of each plugin's relative source before it resolves it, which
// skillsLocalSource joins for the same reason. Its object's keys are checked as every other
// object's are.
func readSkillsPluginRoot(raw json.RawMessage, path string) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	if err := checkSkillsObjectKeys(raw, metadataKeys, "the marketplace catalog's metadata", path); err != nil {
		return "", err
	}
	var metadata struct {
		PluginRoot string `json:"pluginRoot"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", fmt.Errorf("parse the marketplace catalog's metadata in %s: %w", path, err)
	}
	return metadata.PluginRoot, nil
}
