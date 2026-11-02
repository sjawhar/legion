package config

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// repositoryRenamesKey is `repository_renames`, the key every refusal of it names.
const repositoryRenamesKey = "repository_renames"

// readRepositoryRenames is `repository_renames`: a list of mappings, each exactly a `from` and a
// `to` Parse takes, the repository as it was and the one it moved to. It is a top-level key rather
// than a project's, because the equivalence serves the intake filters and a binary-wide comparison,
// neither of which is per project. An entry that is not a mapping, a field that is not one of the
// two, a missing or empty side and a side Parse refuses are each refused naming the entry; the
// structural rules over the whole list are ghrepo.NewRenames' (checkRepositoryRenames), once the
// list is read. An absent key, a null one and an empty list are each no rename, as with `deploys`.
func readRepositoryRenames(value *yaml.Node, key string) ([]ghrepo.Rename, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	var renames []ghrepo.Rename
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		fields, err := members(entry, field, "from", "to")
		if err != nil {
			return nil, err
		}
		from, err := requiredString(fields["from"], field+".from", ": the repository as it was")
		if err != nil {
			return nil, err
		}
		to, err := requiredString(fields["to"], field+".to", ": the repository it moved to")
		if err != nil {
			return nil, err
		}
		var rename ghrepo.Rename
		if rename.From, err = ghrepo.Parse(field+".from", from); err != nil {
			return nil, err
		}
		if rename.To, err = ghrepo.Parse(field+".to", to); err != nil {
			return nil, err
		}
		renames = append(renames, rename)
	}
	return renames, nil
}

// checkRepositoryRenames settles the list `repository_renames` read into the one equivalence every
// consumer takes (ghrepo.Renames), refusing what that value cannot hold — a repository renamed to
// itself, one renamed twice, a chain of two pairs — with the key before the pair ghrepo names. A
// file that configures none settles the zero value, which answers exactly as strings.EqualFold
// does.
func checkRepositoryRenames(file fileConfig, cfg *Config) error {
	renames, err := ghrepo.NewRenames(file.RepositoryRenames)
	if err != nil {
		return fmt.Errorf("%s: %w", repositoryRenamesKey, err)
	}
	cfg.RepositoryRenames = renames
	return nil
}
