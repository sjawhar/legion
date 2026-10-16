package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gcpLatest is the alias of the version Secret Manager reads by default: the newest one.
const gcpLatest = "latest"

var (
	// gcpPrefixPattern is BROKER_SECRETS_PREFIX's form with the GCP store: the project by its
	// number, since Secret Manager answers every resource name with the number and the policy's
	// prefix cut must see what the API answers, and a secret-id prefix ending in "-".
	gcpPrefixPattern = regexp.MustCompile(`^projects/([0-9]+)/secrets/[A-Za-z0-9_-]*-$`)
	// gcpSecretIDPattern is a Secret Manager secret id: 1 to 255 of letters, digits, hyphens and
	// underscores (the REST reference's rule for Secret.name's last segment).
	gcpSecretIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	// gcpKeyNamePattern is a Cloud KMS key's resource name in the global location, the one a
	// secret with automatic replication can be encrypted with.
	gcpKeyNamePattern = regexp.MustCompile(`^projects/[^/\s]+/locations/global/keyRings/[A-Za-z0-9_-]{1,63}/cryptoKeys/[A-Za-z0-9_-]{1,63}$`)
	// gcpKeyVersionSuffix is what a version's kms_key_version_name carries past the key's name.
	gcpKeyVersionSuffix = regexp.MustCompile(`/cryptoKeyVersions/[0-9]+$`)
)

// ValidGCPPrefix refuses a BROKER_SECRETS_PREFIX the GCP store cannot list under:
// projects/<project number>/secrets/<secret-id prefix>-, the project by its number.
func ValidGCPPrefix(prefix string) error {
	if !gcpPrefixPattern.MatchString(prefix) {
		return fmt.Errorf("a Google Secret Manager prefix is projects/<project number>/secrets/<secret-id prefix>-, such as projects/000000000000/secrets/agent-secrets-, got %q (a project id in place of the number is refused: Secret Manager answers every resource name with the number; gcloud projects describe <id> --format='value(projectNumber)' finds it)", prefix)
	}
	return nil
}

// GCPProject answers the project number a prefix ValidGCPPrefix accepts names.
func GCPProject(prefix string) string {
	return gcpPrefixPattern.FindStringSubmatch(prefix)[1]
}

// ValidGCPKeyName refuses a BROKER_SECRETS_KMS_KEY_NAME that is not a Cloud KMS key's resource
// name in the global location: projects/<project>/locations/global/keyRings/<ring>/cryptoKeys/<key>.
// A secret with automatic replication, the only replication the store serves, takes a global key.
func ValidGCPKeyName(name string) error {
	if !gcpKeyNamePattern.MatchString(name) {
		return fmt.Errorf("a Cloud KMS key name is projects/<project>/locations/global/keyRings/<ring>/cryptoKeys/<key>, got %q", name)
	}
	return nil
}

// GCPClient is the four Secret Manager calls the GCP store reads with; *secretmanager.Client
// implements it, and the tests' fake stands in over gRPC behind one.
type GCPClient interface {
	ListSecrets(ctx context.Context, req *secretmanagerpb.ListSecretsRequest, opts ...gax.CallOption) *secretmanager.SecretIterator
	GetSecret(ctx context.Context, req *secretmanagerpb.GetSecretRequest, opts ...gax.CallOption) (*secretmanagerpb.Secret, error)
	GetSecretVersion(ctx context.Context, req *secretmanagerpb.GetSecretVersionRequest, opts ...gax.CallOption) (*secretmanagerpb.SecretVersion, error)
	AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest, opts ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error)
}

// GCP is the Store over Google Cloud Secret Manager.
type GCP struct {
	// Client reads the secrets.
	Client GCPClient
	// KeyName is the agent-secrets key's resource name (BROKER_SECRETS_KMS_KEY_NAME); a secret
	// whose current version is encrypted with any other key, Google's default one included, is
	// not OnKey.
	KeyName string
}

// NewGCPClient opens Secret Manager under Application Default Credentials: on GKE the metadata
// server and Workload Identity Federation for the pod's ServiceAccount, with no key file. The
// client library's logging is off whatever GOOGLE_SDK_GO_LOGGING_LEVEL says: at debug it prints
// every request and response, and a Read's response carries the value.
func NewGCPClient(ctx context.Context) (*secretmanager.Client, error) {
	client, err := secretmanager.NewClient(ctx, option.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		return nil, fmt.Errorf("open Secret Manager: %w", err)
	}
	return client, nil
}

// gcpListLag is how stale Secret Manager's metadata may be. Google documents every operation but
// adding a version and reading it back by number as eventually consistent, converging "typically
// within minutes, but may take a few hours" (Secret Manager's resource consistency reference), for
// ListSecrets and GetSecret alike; six hours covers that bound.
const gcpListLag = 6 * time.Hour

// ListLag is Secret Manager's documented convergence bound for its metadata, six hours.
func (GCP) ListLag() time.Duration { return gcpListLag }

// ValidName refuses a full name whose secret id (the segment after secrets/) is not 1 to 255 of
// letters, digits, hyphens and underscores, the only ids Secret Manager gives a secret.
func (g GCP) ValidName(id string) error {
	_, secretID, ok := strings.Cut(id, "/secrets/")
	if !ok || !gcpSecretIDPattern.MatchString(secretID) {
		return fmt.Errorf("under the prefix it makes the secret id %q, not 1 to 255 letters, digits, hyphens and underscores as Secret Manager requires", secretID)
	}
	return nil
}

// List lists every secret of the prefix's project whose id starts with the prefix's id prefix:
// ListSecrets' name filter (name:<id prefix>) is a case-insensitive substring match, so a listed
// secret whose full name does not start with prefix, byte for byte, is dropped here before any
// call is made for it. It reads every page, then each kept secret's latest version with one
// GetSecretVersion for its state and key. A failed call fails the whole List.
func (g GCP) List(ctx context.Context, prefix string) ([]Listing, error) {
	parent, idPrefix, ok := strings.Cut(prefix, "/secrets/")
	if !ok {
		return nil, fmt.Errorf("list secrets under %s: not a Secret Manager prefix", prefix)
	}
	var listings []Listing
	it := g.Client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: parent, Filter: "name:" + idPrefix})
	for secret, err := range it.All() {
		if err != nil {
			return nil, fmt.Errorf("list secrets under %s: %w", prefix, gcpErr(err))
		}
		if !strings.HasPrefix(secret.GetName(), prefix) {
			continue
		}
		ls, err := g.listing(ctx, secret)
		if err != nil {
			return nil, err
		}
		listings = append(listings, ls)
	}
	return listings, nil
}

// Describe reads one secret with GetSecret, then its latest version with GetSecretVersion:
// ErrNotFound for one Secret Manager does not hold, and Deleted for one with an expire_time:
// `agent-secrets secret delete` schedules a deletion by setting one (Secret Manager deletes the
// secret, with every version, when that time comes), so a secret with one is the counterpart of a
// Secrets Manager secret in its recovery window, which the policy stops serving at once.
func (g GCP) Describe(ctx context.Context, id string) (Listing, error) {
	secret, err := g.Client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: id})
	if status.Code(err) == codes.NotFound {
		return Listing{}, ErrNotFound
	}
	if err != nil {
		return Listing{}, fmt.Errorf("describe %s: %w", id, gcpErr(err))
	}
	return g.listing(ctx, secret)
}

// listing is the Listing of one Secret the API answered, with its latest version read.
func (g GCP) listing(ctx context.Context, secret *secretmanagerpb.Secret) (Listing, error) {
	name := secret.GetName()
	ls := Listing{ID: name, Source: name, Tags: secret.GetAnnotations(), Deleted: secret.GetExpireTime() != nil}
	if ls.Tags == nil {
		ls.Tags = map[string]string{}
	}
	latest := name + "/versions/" + gcpLatest
	version, err := g.Client.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: latest})
	var onKey bool
	switch status.Code(err) {
	case codes.OK:
		ls.HasCurrentValue = version.GetState() == secretmanagerpb.SecretVersion_ENABLED
		// The version's own key is what encrypts the value; a version made before the secret's
		// replication policy was changed stays on the key it was made with.
		onKey = g.onKey(version.GetReplicationStatus().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyVersionName())
	case codes.NotFound:
		// A secret with no version: the policy refuses it for its missing value, unless its
		// replication policy names another key, which a value alone would not fix.
		onKey = secret.GetReplication().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName() == g.KeyName
	default:
		return Listing{}, fmt.Errorf("describe %s: %w", latest, gcpErr(err))
	}
	ls.OnKey = func(context.Context) (bool, error) { return onKey, nil }
	return ls, nil
}

// onKey reports whether keyVersion, a version's kms_key_version_name
// (<key name>/cryptoKeyVersions/<n>), names a version of the agent-secrets key. An empty one is
// Google's default encryption.
func (g GCP) onKey(keyVersion string) bool {
	if keyVersion == "" {
		return false
	}
	return gcpKeyVersionSuffix.ReplaceAllString(keyVersion, "") == g.KeyName
}

// Read answers the secret's current value with AccessSecretVersion on versions/latest, after a
// GetSecret that refuses one scheduled for deletion: Secret Manager reads a secret until its
// expire_time, so without that check a grant made before `agent-secrets secret delete` would keep
// reading it until the policy drops it, where Secrets Manager refuses a secret in its recovery
// window at once. ErrNotFound when Secret Manager holds no such secret or no version (NOT_FOUND),
// when the secret has an expire_time, or when the latest version is disabled or destroyed
// (FAILED_PRECONDITION), all the same answer to the caller: the secret is not readable. The value
// must be non-empty UTF-8, since the broker injects it into an environment; an error names the
// secret and never the bytes.
func (g GCP) Read(ctx context.Context, source string) (string, error) {
	secret, err := g.Client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: source})
	switch status.Code(err) {
	case codes.OK:
		if secret.GetExpireTime() != nil {
			return "", ErrNotFound
		}
	case codes.NotFound:
		return "", ErrNotFound
	default:
		return "", fmt.Errorf("secret manager %s: %w", source, gcpErr(err))
	}
	latest := source + "/versions/" + gcpLatest
	out, err := g.Client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: latest})
	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound, codes.FailedPrecondition:
		return "", ErrNotFound
	default:
		return "", fmt.Errorf("secret manager %s: %w", latest, gcpErr(err))
	}
	data := out.GetPayload().GetData()
	if len(data) == 0 || !utf8.Valid(data) {
		return "", fmt.Errorf("secret manager %s: value is not a non-empty UTF-8 string", latest)
	}
	return string(data), nil
}

// gcpErr is a Secret Manager error as the broker logs it: the call's gRPC status code alone, never
// the status message, which can quote the request or a value.
func gcpErr(err error) error {
	return fmt.Errorf("%s", status.Code(err))
}
