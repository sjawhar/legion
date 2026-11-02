// packages/envoy/internal/brokerclient/signinstore.go
//
// Where a person's sign-in on a machine lives, and which person a session acts for.
//
// `agent-secrets login` makes a box key, runs Google's device grant for the person at the
// keyboard, and keeps the two together in one record: the key the broker binds the sign-in to,
// and the refresh token that mints the person's next identity token. The record is one file per
// person, mode 0600 in a 0700 directory, so that person's own user alone reads it — it holds a
// key and a Google refresh token, which is everything needed to act as them at this broker until
// they revoke it.
//
// One record per person, so a second person on the box signs in beside the first rather than
// over them. Which of them a session acts for is settled by Choose: PersonVar names one; with
// the variable unset and exactly one sign-in on the box, that one; with the variable unset and
// more than one, every form that needs a session identity is refused and told their names, so a
// command never silently reads one colleague's secrets under another's name.
//
// Each file is named for the SHA-256 of the lowercased address rather than the address itself,
// so signing in again as the same person replaces that person's record and two addresses never
// land on one file; the address itself is in the record, which is what every refusal and every
// listing reads.

package brokerclient

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// PersonVar names which person a session acts for where the machine holds more than one
	// sign-in.
	PersonVar = "AGENT_SECRETS_PERSON"
	// SignInCommand is the command every refusal a sign-in would cure names.
	SignInCommand = "agent-secrets login"
)

// ErrNoSignIn is a machine where nobody has signed in.
var ErrNoSignIn = fmt.Errorf("nobody is signed in on this machine; run: %s", SignInCommand)

// SignInRecord is one person's sign-in on this machine as it is kept on disk.
type SignInRecord struct {
	// Person is the address the sign-in proves (the identity token's `email` claim), and the
	// name a session is told to act as.
	Person string `json:"person"`
	// Issuer and ClientID are the provider and the device-flow client the sign-in was made
	// with, so a renewal of the person's identity token goes back to the same place.
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// RefreshToken mints the person's next identity token with no step by them.
	RefreshToken string `json:"refresh_token"`
	// BoxKey is the box key this machine made at sign-in, PKCS#8 PEM: the key the broker bound
	// the sign-in to, which every later trade proves and every proof of this person's session
	// on this machine is signed with.
	BoxKey string `json:"box_key"`
	// Thumbprint is that key's JWK thumbprint, which the broker records for the sign-in.
	Thumbprint string `json:"thumbprint"`
	// EnrollmentID is the enrollment this person's session on this machine registered under the
	// access token the sign-in trades for, empty until one is made.
	EnrollmentID string `json:"enrollment_id,omitempty"`
	// SignedInAt is when the device grant completed.
	SignedInAt time.Time `json:"signed_in_at"`

	// path is the file this record was read from; not part of the record.
	path string
}

// Key is the box key as a key, parsed from the record's PEM.
func (r SignInRecord) Key() (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(r.BoxKey))
	if block == nil {
		return nil, fmt.Errorf("%s's sign-in holds no PEM box key", r.Person)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read %s's box key: %w", r.Person, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s's box key is not an ECDSA private key", r.Person)
	}
	return key, nil
}

// SignInStore is the directory a machine keeps its sign-ins in, one file per person.
type SignInStore struct {
	// Dir is that directory, made 0700 by the first Write.
	Dir string
}

// Path is the file one person's record lives in.
func (s SignInStore) Path(person string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(person))))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:])+".json")
}

// Write keeps a record, making the directory 0700 and the file 0600 so the person's own user
// alone reads the key and the refresh token in it. A second sign-in as the same person replaces
// that person's record and leaves every other person's alone.
func (s SignInStore) Write(record SignInRecord) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("make the directory this machine's sign-ins live in: %w", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return fmt.Errorf("hold %s to this person's own user: %w", s.Dir, err)
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := s.Path(record.Person)
	if err := WriteFileAtomic(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("keep %s's sign-in at %s: %w", record.Person, path, err)
	}
	return nil
}

// Read reads one record.
func (s SignInStore) Read(path string) (SignInRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SignInRecord{}, err
	}
	var record SignInRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return SignInRecord{}, fmt.Errorf("read the sign-in at %s: %w", path, err)
	}
	if record.Person == "" || record.RefreshToken == "" || record.BoxKey == "" {
		return SignInRecord{}, fmt.Errorf("the sign-in at %s names no person, no refresh token or no box key; run: %s", path, SignInCommand)
	}
	record.path = path
	return record, nil
}

// List is every sign-in on this machine, by person. A directory that is not there is no sign-ins
// and no error: most machines have none.
func (s SignInStore) List() ([]SignInRecord, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read this machine's sign-ins: %w", err)
	}
	var records []SignInRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, err := s.Read(filepath.Join(s.Dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b SignInRecord) int { return strings.Compare(a.Person, b.Person) })
	return records, nil
}

// Choose is the sign-in a session acts under, by the rules the file comment states. person is
// what PersonVar says, empty when it says nothing. It is the one place those rules are applied,
// so the sign-in command, the signer and every refusal agree.
func (s SignInStore) Choose(person string) (SignInRecord, error) {
	records, err := s.List()
	if err != nil {
		return SignInRecord{}, err
	}
	person = strings.ToLower(strings.TrimSpace(person))
	switch {
	case len(records) == 0:
		return SignInRecord{}, ErrNoSignIn
	case person != "":
		for _, record := range records {
			if strings.EqualFold(record.Person, person) {
				return record, nil
			}
		}
		return SignInRecord{}, fmt.Errorf("%s names %s, who is not signed in on this machine; signed in here: %s; run: %s",
			PersonVar, person, namesOf(records), SignInCommand)
	case len(records) == 1:
		return records[0], nil
	default:
		return SignInRecord{}, fmt.Errorf("%d people are signed in on this machine (%s), so this session must say which one it acts for: set %s to one of them",
			len(records), namesOf(records), PersonVar)
	}
}

// namesOf lists the people signed in here, for a refusal to name them.
func namesOf(records []SignInRecord) string {
	names := make([]string, len(records))
	for i, record := range records {
		names[i] = record.Person
	}
	return strings.Join(names, ", ")
}

// WriteFileAtomic writes data to path by renaming a finished temporary file into place, so no
// reader ever sees path half-written: a sign-in record is read back by the next command while
// this one is still running, and a call waiting out a box's enrollment (cmd/agent-secrets'
// awaitEnrollment) reads key.pem and enrollment the moment each appears.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has taken it
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
