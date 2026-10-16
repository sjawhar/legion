package seed

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/bus"
)

// GCPSecretManager is the --store value of Google Cloud Secret Manager.
const GCPSecretManager = "gcp-secret-manager"

// GCP is the store type --store gcp-secret-manager names.
var GCP = StoreType{
	Open:      OpenGCP,
	CheckName: gcpName,
	Help: "Google Cloud Secret Manager. --name is projects/<project>/secrets/<secret>, which names its " +
		"project; --region and --expect-account are refused.",
}

// gcpLatest is the alias of the version Secret Manager serves by default: the newest one, whatever
// its state.
const gcpLatest = "latest"

var (
	gcpSecretName  = regexp.MustCompile(`^projects/[^/\s]+/secrets/[A-Za-z0-9_-]{1,255}$`)
	gcpVersionName = regexp.MustCompile(`^projects/[^/]+/secrets/[^/]+/versions/([0-9]+)$`)
)

// gcpConfigEnv are the environment variables whose value the store, or the libraries it calls,
// send in a request, dial as a host or quote in an error: the access token, sent as the bearer
// token; the key file's path, quoted when it cannot be opened (cloud.google.com/go/auth v0.22.0
// internal/credsfile); the quota project, sent as x-goog-user-project (internal/internal.go:110);
// the universe domain, the default endpoint's host (grpctransport); the metadata server's host
// (cloud.google.com/go/compute/metadata v0.9.0 metadata.go); and the endpoint override.
var gcpConfigEnv = []string{
	"GOOGLE_OAUTH_ACCESS_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_QUOTA_PROJECT",
	"GOOGLE_CLOUD_UNIVERSE_DOMAIN", "GCE_METADATA_HOST", "SECRET_MANAGER_ENDPOINT",
}

// OpenGCP opens Secret Manager under Application Default Credentials, or under the access token in
// GOOGLE_OAUTH_ACCESS_TOKEN when that is set, at the endpoint SECRET_MANAGER_ENDPOINT names when it is
// set. It refuses --region and --expect-account, which are Secrets Manager's: a Secret Manager
// secret is global, and its name names its project.
//
// A variable of gcpConfigEnv whose value holds an nkey seed (bus.HoldsSeed: any type, either case,
// whole, inside a longer value such as a path, a token or a host, split by '.', '-', '_' or
// whitespace, or percent-encoded) is refused first, naming the variable and never the value, as the
// AWS store refuses a seed in its configuration. The check covers SECRET_MANAGER_ENDPOINT's whole
// value, its host, path and query included, so it comes before the endpoint's form, whose refusal
// quotes nothing either.
func OpenGCP(ctx context.Context, region, account string) (Store, error) {
	if account != "" {
		return nil, refuse("--expect-account is not accepted with --store %s: it checks an AWS account, and the secret's name names its project", GCPSecretManager)
	}
	if region != "" {
		return nil, refuse("--region is not accepted with --store %s: its secrets are global, named projects/<project>/secrets/<secret>", GCPSecretManager)
	}
	for _, name := range gcpConfigEnv {
		if bus.HoldsSeed(os.Getenv(name)) {
			return nil, gcpConfigSeed(name)
		}
	}
	opts, err := gcpClientOptions()
	if err != nil {
		return nil, err
	}
	client, err := secretmanager.NewClient(ctx, opts...)
	if err != nil {
		return nil, refuse("open Secret Manager: %v", err)
	}
	return &gcpStore{client: client}, nil
}

// gcpClientOptions turn the client library's logging off, whatever GOOGLE_SDK_GO_LOGGING_LEVEL says:
// at debug it prints every request and response, and a write carries the seed in one, a read in the
// other. They take the access token in GOOGLE_OAUTH_ACCESS_TOKEN when that is set, and the endpoint
// in SECRET_MANAGER_ENDPOINT.
func gcpClientOptions() ([]option.ClientOption, error) {
	opts := []option.ClientOption{option.WithLogger(slog.New(slog.DiscardHandler))}
	if token, ok := os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN"); ok {
		if token == "" {
			return nil, refuse("GOOGLE_OAUTH_ACCESS_TOKEN is set but empty; unset it to use Application Default Credentials")
		}
		opts = append(opts, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})))
	}
	if raw, ok := os.LookupEnv("SECRET_MANAGER_ENDPOINT"); ok {
		endpoint, err := gcpEndpoint(raw)
		if err != nil {
			return nil, err
		}
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	return opts, nil
}

// gcpEndpoint turns raw, https://<host>[:<port>] as for Secret Manager's REST API, into the host:port
// the gRPC client dials, port 443 by default. It takes https alone, with no exception for this
// machine's loopback as the AWS store's endpointCheck has: the client dials every endpoint with TLS
// (google.golang.org/api transport/grpc DialPool), so a seed is never sent in plain text, and an
// http endpoint could only fail its handshake.
func gcpEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" {
		return "", refuse("SECRET_MANAGER_ENDPOINT is https://<host>[:<port>], as https://secretmanager.googleapis.com")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// gcpConfigSeed refuses a value of the Google Cloud configuration that holds an nkey user seed,
// naming source, where it came from.
func gcpConfigSeed(source string) error {
	return refuse("%s: an nkey user seed, which is never Google Cloud configuration (not echoed)", source)
}

// gcpStore is Store over Secret Manager's gRPC API. Every error names the call, the resource and the
// gRPC status code, never the status message, which can quote the request.
type gcpStore struct{ client *secretmanager.Client }

// Label is latest.
func (s *gcpStore) Label() string { return gcpLatest }

// Describe reads GetSecret, then GetSecretVersion on versions/latest for the newest version's number
// and state: metadata alone. A secret with an expire_time is scheduled for deletion: Secret Manager
// deletes it, with every version, when that time comes. The newest version is the default, with its
// state unless it is ENABLED, the one state AccessSecretVersion reads.
func (s *gcpStore) Describe(ctx context.Context, name string) (Description, error) {
	if err := gcpName(name); err != nil {
		return Description{}, err
	}
	secret, err := s.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if status.Code(err) == codes.NotFound {
		return Description{}, nil
	}
	if err != nil {
		return Description{}, failed("get-secret", name, err)
	}
	if expire := secret.GetExpireTime(); expire != nil {
		return Description{Exists: true, ScheduledForDeletion: true, DeletionDate: pythonDate(expire.AsTime(), time.Local)}, nil
	}
	d := Description{Exists: true}
	latest := name + "/versions/" + gcpLatest
	newest, err := s.client.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: latest})
	if status.Code(err) == codes.NotFound { // a secret that exists, with no version
		return d, nil
	}
	if err != nil {
		return Description{}, failed("get-secret-version", latest, err)
	}
	v, err := gcpDefault("get-secret-version", latest, newest)
	if err != nil {
		return Description{}, err
	}
	d.Default = &v
	return d, nil
}

// gcpDefault is the Version a SecretVersion that call answered for resource names.
func gcpDefault(call, resource string, v *secretmanagerpb.SecretVersion) (Version, error) {
	number, err := gcpVersionNumber(call, resource, v.GetName())
	if err != nil {
		return Version{}, err
	}
	if v.GetState() == secretmanagerpb.SecretVersion_ENABLED {
		return Version{ID: number}, nil
	}
	return Version{ID: number, State: v.GetState().String()}, nil
}

// Versions counts every version ListSecretVersions lists with no filter, in every state, destroyed
// ones included, across every page: a destroyed version held a seed a client may have read. It
// pages by hand with InternalFetch, which secretmanager v1.22.0 marks outside its stable interface
// (apiv1/auxiliary.go), since the library's iterator follows a repeated page token forever.
//
// The listing, like versions/latest, is eventually consistent with an add, but an access by version
// number is not, and every version is numbered from 1: so when the listing shows none, Versions
// counts 1 when versions/1 exists in any state.
func (s *gcpStore) Versions(ctx context.Context, name string) (int, error) {
	if err := gcpName(name); err != nil {
		return 0, err
	}
	listing := s.client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: name})
	count, err := countVersions("list-secret-versions", name, func(token string) (int, string, error) {
		page, next, err := listing.InternalFetch(0, token)
		if err != nil {
			return 0, "", failed("list-secret-versions", name, err)
		}
		return len(page), next, nil
	})
	if err != nil || count > 0 {
		return count, err
	}
	holds, err := s.holds(ctx, name, "1")
	if err != nil || !holds {
		return 0, err
	}
	return 1, nil
}

// holds reads whether the secret holds version number, in any state, with AccessSecretVersion by
// that number, the one read Secret Manager keeps strongly consistent with the add
// (https://cloud.google.com/secret-manager/docs/access-secret-version, "Resource consistency"):
// NOT_FOUND is no such version, FAILED_PRECONDITION a disabled or destroyed one. A value read is
// cleared at once.
func (s *gcpStore) holds(ctx context.Context, name, number string) (bool, error) {
	version := name + "/versions/" + number
	read, err := s.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version})
	switch status.Code(err) {
	case codes.OK:
		clear(read.GetPayload().GetData())
		return true, nil
	case codes.NotFound:
		return false, nil
	case codes.FailedPrecondition:
		return true, nil
	}
	return false, failed("access-secret-version", version, err)
}

// Put adds value as the secret's first version with AddSecretVersion. AddSecretVersion takes no
// request id and no precondition, so Put first reads version 1 by number, the one read Secret
// Manager keeps strongly consistent with an add, and refuses with ErrRefused, writing nothing, when
// version 1 exists in any state: another generate wrote this secret's first version after this
// one's checks. The library does not retry AddSecretVersion, so a lost answer leaves the write
// unknown rather than doubled. The added version must be version 1, since Secret Manager numbers a
// secret's versions from 1, one more for each (resources.proto, SecretVersion.name): any other
// number is another generate's version that landed between the read and the add, the window
// gcpOvertaken describes. It must be ENABLED, and read back by its number as value.
func (s *gcpStore) Put(ctx context.Context, name string, value []byte) (string, error) {
	if err := gcpName(name); err != nil {
		return "", err
	}
	first := name + "/versions/1"
	taken, err := s.holds(ctx, name, "1")
	if err != nil {
		return "", refuse("%v; nothing was written", err)
	}
	if taken {
		return "", refuse("add-secret-version %s: %s already exists; another generate wrote this secret's first version, "+
			"with another seed", name, first)
	}
	added, err := s.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  name,
		Payload: &secretmanagerpb.SecretPayload{Data: value},
	})
	if err != nil {
		return "", failed("add-secret-version", name, err)
	}
	number, err := gcpVersionNumber("add-secret-version", name, added.GetName())
	if err != nil {
		return "", err
	}
	if number != "1" {
		return number, gcpOvertaken(name, number)
	}
	if added.GetState() != secretmanagerpb.SecretVersion_ENABLED {
		return number, fmt.Errorf("version %s was written but is %s; run check", number, added.GetState())
	}
	version := name + "/versions/" + number
	read, err := s.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version})
	if err != nil {
		return number, fmt.Errorf("version %s was written, but reading it back by number failed: %v; run check",
			number, failed("access-secret-version", version, err))
	}
	stored := read.GetPayload().GetData()
	defer clear(stored)
	if !bytes.Equal(stored, value) {
		return number, fmt.Errorf("version %s was written, but %s reads back another value; run check", number, version)
	}
	return number, nil
}

// gcpOvertaken is the error of a Put whose add landed as version number, not 1: another generate's
// add landed between this Put's read of version 1 and its own. Secret Manager has no conditional
// add, and the store takes this window over a claim on the secret (an etag-guarded UpdateSecret
// before the add), which would need secretmanager.secrets.update and leave a dead run's claim for
// someone to clear. The error says, in order, what happened, what it means for the other run's
// key, and the commands that recover: keep this run's seed, or copy version 1 to a new version
// and destroy this one, since latest is the most recently created version whatever its state
// (secretmanagerpb service.pb.go, AccessSecretVersionRequest.name), so disabling or destroying
// this version alone would leave no seed served.
func gcpOvertaken(name, number string) error {
	mine := name + "/versions/" + number
	first := name + "/versions/1"
	return fmt.Errorf("version %[2]s was written, not version 1: another generate wrote version 1 of %[1]s between "+
		"this run's read of version 1 and its write, so this run's version %[2]s is now the newest, the one latest serves.\n"+
		"The public key the other run printed does not derive the served seed, so a client given it is refused; the "+
		"public key above does.\n"+
		"To keep this run's seed, put the public key above in the NATS authorization in place of the other run's, then "+
		"destroy the other run's version:\n"+
		"  gcloud secrets versions destroy %[3]s\n"+
		"To keep the other run's seed instead, copy version 1 to a new version, then destroy this run's (destroying or "+
		"disabling version %[2]s alone leaves latest naming it, and no seed is served):\n"+
		"  gcloud secrets versions access %[3]s --format='get(payload.data)' | tr '_-' '/+' | base64 -d | gcloud "+
		"secrets versions add %[1]s --data-file=-\n"+
		"  gcloud secrets versions destroy %[4]s\n"+
		"Then check the secret, and verify the key you kept:\n"+
		"  envoy-infra seed check --store %[5]s --name %[1]s\n"+
		"  envoy-infra seed verify --store %[5]s --name %[1]s --public-key <the key you kept>",
		name, number, first, mine, GCPSecretManager)
}

// Current reads versions/latest with AccessSecretVersion. Secret Manager answers NOT_FOUND when
// latest names no version, as just after a first write it may: an access through latest is
// eventually consistent with the write. It answers FAILED_PRECONDITION when latest names a version
// that is not ENABLED, as one disabled after Describe read it is; one GetSecretVersion then names
// that version in an *Unserved.
func (s *gcpStore) Current(ctx context.Context, name string) (string, []byte, error) {
	if err := gcpName(name); err != nil {
		return "", nil, err
	}
	latest := name + "/versions/" + gcpLatest
	read, err := s.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: latest})
	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound:
		return "", nil, notServed{failed("access-secret-version", latest, err)}
	case codes.FailedPrecondition:
		refused := failed("access-secret-version", latest, err)
		newest, gerr := s.client.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: latest})
		if gerr != nil {
			return "", nil, refused
		}
		if v, verr := gcpDefault("get-secret-version", latest, newest); verr == nil && v.State != "" {
			return "", nil, &Unserved{Version: v, Err: refused}
		}
		return "", nil, refused
	default:
		return "", nil, failed("access-secret-version", latest, err)
	}
	value := read.GetPayload().GetData()
	number, err := gcpVersionNumber("access-secret-version", latest, read.GetName())
	if err != nil {
		clear(value)
		return "", nil, err
	}
	return number, value, nil
}

// gcpName refuses a name that holds an nkey seed (seededName) or is not a global secret's, without
// quoting it: a seed may have been pasted there. It is the command's name rule for the store and
// the first check of each of the store's methods, so a store a program opens itself refuses one
// too; OpenGCP takes no name.
func gcpName(name string) error {
	if err := seededName(name); err != nil {
		return err
	}
	if !gcpSecretName.MatchString(name) {
		return fmt.Errorf("--name for --store %s is a secret's name, projects/<project>/secrets/<secret>", GCPSecretManager)
	}
	return nil
}

// gcpVersionNumber is the number at the end of a version's name, which call answered for resource.
func gcpVersionNumber(call, resource, version string) (string, error) {
	match := gcpVersionName.FindStringSubmatch(version)
	if match == nil {
		return "", fmt.Errorf("%s %s answered no version number", call, resource)
	}
	return match[1], nil
}
