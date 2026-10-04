package policy

import (
	"errors"
	"regexp"
	"strings"
)

// namePattern is the name a session asks for: the uppercase form of slugPattern (load.go).
var namePattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)

// ErrNameInvalid is a name no secret under the prefix can carry, so no lookup is made for it.
var ErrNameInvalid = errors.New("not an agent secret name: uppercase letters, digits and single underscores, starting with a letter")

// SlugToName is the name a session asks for the secret whose name under the prefix is slug:
// uppercased, each hyphen an underscore (deel-api-key is DEEL_API_KEY).
func SlugToName(slug string) string {
	return strings.ToUpper(strings.ReplaceAll(slug, "-", "_"))
}

// NameToSlug is the secret's name under the prefix for name, SlugToName's inverse; a name that is
// not uppercase letters, digits and single underscores starting with a letter is ErrNameInvalid.
func NameToSlug(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", ErrNameInvalid
	}
	return slugOf(name), nil
}

// slugOf is the slug of name, a Name a load produced and so already of namePattern's form.
func slugOf(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}
