package seed

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GCPSecretManager is the --store value of Google Cloud Secret Manager.
const GCPSecretManager = "gcp-secret-manager"

// gcpLatest is the alias of the version Secret Manager serves by default: the newest one, whatever
// its state.
const gcpLatest = "latest"

var (
	gcpSecretName  = regexp.MustCompile(`^projects/[^/\s]+/secrets/[A-Za-z0-9_-]{1,255}$`)
	gcpVersionName = regexp.MustCompile(`^projects/[^/]+/secrets/[^/]+/versions/([0-9]+)$`)
)

// OpenGCP opens Secret Manager under Application Default Credentials, or under the access token in
// GOOGLE_OAUTH_ACCESS_TOKEN when that is set, at the endpoint SECRET_MANAGER_ENDPOINT names when it is
// set. It refuses --region and --expect-account, which are Secrets Manager's: a Secret Manager
// secret is global, and its name names its project.
func OpenGCP(ctx context.Context, region, account string) (Store, error) {
	if account != "" {
		return nil, refuse("--expect-account is not accepted with --store %s: it checks an AWS account, and the secret's name names its project", GCPSecretManager)
	}
	if region != "" {
		return nil, refuse("--region is not accepted with --store %s: its secrets are global, named projects/<project>/secrets/<secret>", GCPSecretManager)
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
// the gRPC client dials, port 443 by default.
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

// gcpStore is Store over Secret Manager's gRPC API. Every error names the call, the resource and the
// gRPC status code, never the status message, which can quote the request.
type gcpStore struct{ client *secretmanager.Client }

// Label is latest.
func (s *gcpStore) Label() string { return gcpLatest }

// Describe reads GetSecret, then GetSecretVersion on versions/latest for the newest version's number
// and state: metadata alone. It names that version as the one served only while it is ENABLED, the
// one state AccessSecretVersion reads.
func (s *gcpStore) Describe(ctx context.Context, name string) (Description, error) {
	if err := gcpName(name); err != nil {
		return Description{}, err
	}
	_, err := s.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if status.Code(err) == codes.NotFound {
		return Description{}, nil
	}
	if err != nil {
		return Description{}, gcpFailed("get-secret", name, err)
	}
	d := Description{Exists: true, DefaultIsNewest: true}
	latest := name + "/versions/" + gcpLatest
	newest, err := s.client.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: latest})
	if status.Code(err) == codes.NotFound { // a secret that exists, with no version
		return d, nil
	}
	if err != nil {
		return Description{}, gcpFailed("get-secret-version", latest, err)
	}
	number, err := gcpVersionNumber("get-secret-version", latest, newest.GetName())
	if err != nil {
		return Description{}, err
	}
	d.NewestVersion, d.NewestState = number, newest.GetState().String()
	if newest.GetState() == secretmanagerpb.SecretVersion_ENABLED {
		d.CurrentVersion = number
	}
	return d, nil
}

// Versions counts every version ListSecretVersions lists with no filter, in every state, destroyed
// ones included, across every page: a destroyed version held a seed a client may have read. It
// pages by hand, since the library's iterator follows a repeated page token forever, and refuses one.
func (s *gcpStore) Versions(ctx context.Context, name string) (int, error) {
	if err := gcpName(name); err != nil {
		return 0, err
	}
	listing := s.client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: name})
	handedOut := map[string]bool{}
	count, token := 0, ""
	for {
		page, next, err := listing.InternalFetch(0, token)
		if err != nil {
			return 0, gcpFailed("list-secret-versions", name, err)
		}
		count += len(page)
		if next == "" {
			return count, nil
		}
		if handedOut[next] {
			return 0, fmt.Errorf("list-secret-versions %s failed: the listing handed out page token %s twice", name, next)
		}
		handedOut[next] = true
		token = next
	}
}

// Put adds value as a version with AddSecretVersion, which takes no request id and which the library
// does not retry, so a lost answer leaves the write unknown rather than doubled. The added version
// must be version 1, since Secret Manager numbers a secret's versions from 1, one more for each
// (resources.proto, SecretVersion.name): any other number is a version the eventually consistent
// listing before the write missed. It must also be ENABLED.
func (s *gcpStore) Put(ctx context.Context, name string, value []byte) (string, error) {
	if err := gcpName(name); err != nil {
		return "", err
	}
	added, err := s.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  name,
		Payload: &secretmanagerpb.SecretPayload{Data: value},
	})
	if err != nil {
		return "", gcpFailed("add-secret-version", name, err)
	}
	number, err := gcpVersionNumber("add-secret-version", name, added.GetName())
	if err != nil {
		return "", err
	}
	if number != "1" {
		return number, fmt.Errorf("version %s was written, not version 1: %s held a version the listing before "+
			"the write did not show; run check", number, name)
	}
	if added.GetState() != secretmanagerpb.SecretVersion_ENABLED {
		return number, fmt.Errorf("version %s was written but is %s; run check", number, added.GetState())
	}
	return number, nil
}

// Current reads versions/latest with AccessSecretVersion. Secret Manager answers NOT_FOUND when
// latest names no version, as just after a first write it may: an access through latest is
// eventually consistent with the write.
func (s *gcpStore) Current(ctx context.Context, name string) (string, []byte, error) {
	if err := gcpName(name); err != nil {
		return "", nil, err
	}
	latest := name + "/versions/" + gcpLatest
	read, err := s.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: latest})
	if status.Code(err) == codes.NotFound {
		return "", nil, notServed{gcpFailed("access-secret-version", latest, err)}
	}
	if err != nil {
		return "", nil, gcpFailed("access-secret-version", latest, err)
	}
	value := read.GetPayload().GetData()
	number, err := gcpVersionNumber("access-secret-version", latest, read.GetName())
	if err != nil {
		clear(value)
		return "", nil, err
	}
	return number, value, nil
}

// gcpName refuses a name that is not a global secret's, without quoting it: a seed may have been
// pasted there.
func gcpName(name string) error {
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

// gcpFailed is "<call> <resource> failed: <code>": the gRPC status code, or, for a context's end or
// an error without a status, what errorClass names it.
func gcpFailed(call, resource string, err error) error {
	if s, ok := status.FromError(err); ok {
		return fmt.Errorf("%s %s failed: %s", call, resource, s.Code())
	}
	return fmt.Errorf("%s %s failed: %s", call, resource, errorClass(err))
}
