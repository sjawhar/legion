// packages/envoy/cmd/agent-secrets/secret_gcp.go
//
// The GCP side of the secret forms: the Secret Manager calls they make, under the person's own
// Google sign-in - their Application Default Credentials, or their gcloud login where those are
// absent or, for a write, a machine's own identity (gcloudLogin) - and the check that refuses a
// write under any identity but a person's own (GCP, unlike AWS, needs no separate wrong-project
// read check: every call already names the project explicitly, so the API itself refuses one in
// another project).
//
// GCP Secret Manager has no soft-delete-with-recovery-window the way AWS does. Instead delete sets
// the secret's own expire_time, which GCP uses as a TTL: the secret (and every version it holds)
// stays readable and restorable - clear expire_time to bring it back - until that moment, and GCP
// deletes it for good once it passes, with no further restore. delete therefore sets expire_time
// to now plus recoveryWindowDays (the same window AWS's delete schedules), and restore clears it;
// the moment delete sets is both "when it is deleted" and "the last moment it can be restored",
// unlike AWS's separate DeletedDate and 7-day minimum recovery window, so secretView.EarliestPurge
// is left unset here (list/show print only the deletion moment).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// checkSettingsKMSKeyName refuses, before any GCP call, settings whose secrets_prefix is not a
// Secret Manager prefix, whose gcp_project does not match the project number secrets_prefix
// itself names (the broker derives gcp_project from secrets_prefix the same way,
// internal/broker/api handlers_secrets.go, secrets.GCPProject), or whose kms_key_name is not a
// global-location CMEK key name (secrets.ValidGCPKeyName: automatic replication, the only
// replication the store serves, takes a global key). This is the GCP counterpart of
// checkSettingsKeyARN: it ties the project a write lands in to the resource-name prefix the
// broker itself derived it from, not to a separately-sent field a spoofed or plain-http broker
// could set on its own to steer a write.
func checkSettingsKMSKeyName(settings Settings) error {
	if err := secrets.ValidGCPPrefix(settings.SecretsPrefix); err != nil {
		return fmt.Errorf("the broker's settings name an invalid secrets_prefix: %w", err)
	}
	if project := secrets.GCPProject(settings.SecretsPrefix); settings.GCPProject != project {
		return fmt.Errorf("the broker's settings name project %s, but its secrets prefix %s is in project %s: the broker's settings are inconsistent", settings.GCPProject, settings.SecretsPrefix, project)
	}
	if err := secrets.ValidGCPKeyName(settings.KMSKeyName); err != nil {
		return fmt.Errorf("the broker's settings name an invalid KMS key name: %w", err)
	}
	return nil
}

// gcpCredentials loads the person's Application Default Credentials. Tests replace it.
var gcpCredentials = func(ctx context.Context) (*google.Credentials, error) {
	return google.FindDefaultCredentials(ctx, secretmanager.DefaultAuthScopes()...)
}

// gcpUserinfo reads the signed-in email behind creds from Google's userinfo endpoint.
// Tests replace it so no form makes a network call for it.
var gcpUserinfo = func(ctx context.Context, creds *google.Credentials) (string, error) {
	httpClient := oauth2.NewClient(ctx, creds.TokenSource)
	resp, err := httpClient.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return "", fmt.Errorf("read your Google sign-in's identity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("read your Google sign-in's identity: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var userinfo struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
		return "", fmt.Errorf("decode your Google sign-in's identity: %w", err)
	}
	return userinfo.Email, nil
}

// gcpClientDial builds the Secret Manager client for creds. Tests replace it to dial a local fake
// Secret Manager server instead of the real API. option.WithLogger discards the SDK's own debug
// log (GOOGLE_SDK_GO_LOGGING_LEVEL): at debug level the SDK logs every request as protojson,
// which for AddSecretVersion includes the secret's value, base64-encoded, in Payload.Data (the
// broker's own GCP store takes the same precaution, internal/broker/secrets/gcp.go).
var gcpClientDial = func(ctx context.Context, creds *google.Credentials) (*secretmanager.Client, error) {
	return secretmanager.NewClient(ctx, option.WithCredentials(creds), option.WithLogger(slog.New(slog.DiscardHandler)))
}

// gcloudTokenLife is how long this command takes an access token `gcloud auth print-access-token`
// prints to last, counted from the moment it starts gcloud: gcloud prints the token alone, never
// its expiry, and refreshes a token that would expire within 300 seconds before it prints one
// (_CREDENTIALS_EXPIRY_WINDOW in the Cloud SDK's googlecloudsdk/core/credentials/store.py), so
// every token it prints lasts at least this long. oauth2.ReuseTokenSource asks gcloud again once
// it has passed, and gcloud answers the token it holds until that one nears its own expiry.
const gcloudTokenLife = 5 * time.Minute

// gcloudLogin lends this command the person's own gcloud login (`gcloud auth login`) as Google
// credentials, through the interface gcloud offers for that, never its credential files:
// chooseGcloudAccount picks the account from `gcloud auth list`, and gcloudToken runs
// `gcloud auth print-access-token` for it, gcloud refreshing the login as it does for itself.
// gcloud is the first `gcloud` on PATH. It asks for the first token at once, so a login gcloud
// cannot lend is refused here, before any call is made under it. It answers the account too: a
// write checks that the token is that account's (requireWriteSignIn).
func gcloudLogin(ctx context.Context) (*google.Credentials, gcloudAccount, error) {
	gcloud, err := exec.LookPath("gcloud")
	if err != nil {
		return nil, gcloudAccount{}, errors.New("no gcloud is on PATH: install the Google Cloud CLI, then sign in once with `gcloud auth login`, which leaves this machine's own credentials as they are")
	}
	account, err := chooseGcloudAccount(ctx, gcloud)
	if err != nil {
		return nil, gcloudAccount{}, err
	}
	source := gcloudToken{ctx: ctx, gcloud: gcloud, account: account.email}
	first, err := source.Token()
	if err != nil {
		return nil, gcloudAccount{}, err
	}
	return &google.Credentials{TokenSource: oauth2.ReuseTokenSource(first, source)}, account, nil
}

// gcloudAccount is the gcloud login a command signs in with: its account, as `gcloud auth list`
// names it, and whether gcloud's own CLOUDSDK_CORE_ACCOUNT chose it.
type gcloudAccount struct {
	email string
	named bool
}

// personAccount reports whether account, as `gcloud auth list` names it, is a person's: a plain
// email the broker accepts as a person owner (policy.ValidPersonOwner, lowercased), and no service
// account's (one ending in .gserviceaccount.com, such as a machine's own). A login from a
// credential configuration (`gcloud auth login --cred-file`) is named by its service account's
// email or, for workload and workforce identity federation, by a principal:// identifier, which
// is no email.
func personAccount(account string) bool {
	lower := strings.ToLower(account)
	return policy.ValidPersonOwner(lower) && !strings.HasSuffix(lower, ".gserviceaccount.com")
}

// chooseGcloudAccount picks the person's own login from `gcloud auth list`: of the accounts gcloud
// holds a login for, the people's (personAccount). It takes the one gcloud's own
// CLOUDSDK_CORE_ACCOUNT names when that is one of them, else the only one, and refuses none or
// several: which person a write runs as is never guessed.
func chooseGcloudAccount(ctx context.Context, gcloud string) (gcloudAccount, error) {
	out, err := runGcloud(ctx, gcloud, "auth", "list", "--format=json")
	if err != nil {
		return gcloudAccount{}, fmt.Errorf("`gcloud auth list` failed: %w", err)
	}
	var listed []struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return gcloudAccount{}, fmt.Errorf("`gcloud auth list --format=json` answered something other than a list of accounts: %w", err)
	}
	var people []string
	for _, entry := range listed {
		if personAccount(entry.Account) {
			people = append(people, entry.Account)
		}
	}
	if named := os.Getenv("CLOUDSDK_CORE_ACCOUNT"); named != "" && slices.Contains(people, named) {
		return gcloudAccount{email: named, named: true}, nil
	}
	switch len(people) {
	case 0:
		return gcloudAccount{}, errors.New("gcloud holds no person's login: sign in once with `gcloud auth login`, which leaves this machine's own credentials as they are")
	case 1:
		return gcloudAccount{email: people[0]}, nil
	}
	return gcloudAccount{}, fmt.Errorf("gcloud holds several people's logins (%s), and the command signs in with the one CLOUDSDK_CORE_ACCOUNT names: run it again with CLOUDSDK_CORE_ACCOUNT set to yours", strings.Join(people, ", "))
}

// gcloudToken is the oauth2.TokenSource of one gcloud login: each Token runs
// `gcloud auth print-access-token` for its account and takes the token to last gcloudTokenLife.
// No error carries the token: a failure names the account and what gcloud wrote on its standard
// error.
type gcloudToken struct {
	ctx     context.Context
	gcloud  string
	account string
}

// gcloudTokenArgs is the `gcloud auth print-access-token` command line for account, less gcloud.
// gcloud prints a command's output through its logger, whose file handler keeps a copy in its log
// file (calliope/display.py's printer writes to log.out); the format's private attribute writes
// the token to the console stream alone (core/resource/resource_printer_base.py). The three empty
// flags clear the properties that make gcloud print another identity's token than account's -
// impersonation, an access token file and a credential file override (credentials/store.py's
// Load and _Load) - wherever they are set: a flag outranks the environment and the configuration
// file, and an empty value turns the property off (core/properties.py,
// _GetPropertyWithoutCallback).
func gcloudTokenArgs(account string) []string {
	return []string{
		"auth", "print-access-token", "--format=value[private](token)",
		"--impersonate-service-account=", "--access-token-file=", "--credential-file-override=",
		account,
	}
}

func (s gcloudToken) Token() (*oauth2.Token, error) {
	started := time.Now()
	out, err := runGcloud(s.ctx, s.gcloud, gcloudTokenArgs(s.account)...)
	if err != nil {
		return nil, fmt.Errorf("`gcloud auth print-access-token` failed for %s: %w", s.account, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("`gcloud auth print-access-token` answered something other than one access token for %s", s.account)
	}
	return &oauth2.Token{AccessToken: token, TokenType: "Bearer", Expiry: started.Add(gcloudTokenLife)}, nil
}

// gcloudIdentityOverrides are the variables that make gcloud lend another identity than the
// account it is asked for: an access token (store.ACCESS_TOKEN_ENV_VAR_NAME, read by _Load alone),
// and the auth properties an access token file, a credential file override and impersonation
// set. runGcloud runs gcloud without them; gcloudTokenArgs also clears the last three where the
// configuration file sets them.
var gcloudIdentityOverrides = []string{
	"CLOUDSDK_AUTH_ACCESS_TOKEN",
	"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE",
	"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
	"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT",
}

// gcloudEnv is the environment gcloud runs in: this command's, less gcloudIdentityOverrides, with
// prompts off, so gcloud fails rather than waits on one, file logging off
// (core/disable_file_logging: log.py's AddLogsDir adds no file handler), so no run leaves a log
// file behind, and HTTP logging off (core/log_http), whatever this command's environment or
// gcloud's configuration sets: with it on, and its token redaction off, a failed refresh prints
// its request body, the refresh token in it, on the standard error runGcloud quotes in its
// errors. A variable outranks the configuration file, and given twice takes its last value.
func gcloudEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains(gcloudIdentityOverrides, name)
	})
	return append(env, "CLOUDSDK_CORE_DISABLE_PROMPTS=1", "CLOUDSDK_CORE_DISABLE_FILE_LOGGING=1", "CLOUDSDK_CORE_LOG_HTTP=false")
}

// runGcloud runs gcloud with args in gcloudEnv and answers what it wrote on its standard output.
// gcloud reads /dev/null, never this command's standard input, which can be a secret's value on
// its way in; what it writes on its standard error is kept only to name a failure.
func runGcloud(ctx context.Context, gcloud string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gcloud, args...)
	cmd.Env = gcloudEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if said := strings.TrimSpace(stderr.String()); said != "" {
			return nil, fmt.Errorf("%w: %s", err, said)
		}
		return nil, err
	}
	return out, nil
}

// newGCPBackend loads the person's Application Default Credentials, reads the ADC JSON's own
// "type" and dials the Secret Manager client (gcpClientDial). Where there are none to load and
// GOOGLE_APPLICATION_CREDENTIALS is unset, every form, a read included, signs in with the
// person's gcloud login (gcloudLogin) instead. It makes no userinfo call: that is only needed for
// a write's --owner me, so requireWriteSignIn makes it lazily, and a read under an identity whose
// token lacks userinfo scope (common for a machine's) still succeeds.
func newGCPBackend(ctx context.Context, settings Settings) (*gcpBackend, error) {
	b := &gcpBackend{settings: settings}
	creds, err := gcpCredentials(ctx)
	switch {
	case err != nil && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "":
		// google.FindDefaultCredentials reads only the file GOOGLE_APPLICATION_CREDENTIALS names
		// whenever that is set, so no sign-in fixes a file it cannot read or parse: only that
		// variable or its file does.
		return nil, fmt.Errorf("load the Google Cloud credentials file GOOGLE_APPLICATION_CREDENTIALS names: %w", err)
	case err != nil:
		// The load's own error is left out: it names the file it read, or links Google's guide to
		// `gcloud auth application-default login`, which replaces a machine's own credentials for
		// every program that uses them, and the gcloud login needs neither.
		var account gcloudAccount
		if creds, account, err = gcloudLogin(ctx); err != nil {
			return nil, fmt.Errorf("found no usable Google Cloud Application Default Credentials, so this command uses your gcloud login, but %w", err)
		}
		b.login = account
	case len(creds.JSON) > 0:
		var parsed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(creds.JSON, &parsed); err == nil {
			b.credsKind = google.CredentialsType(parsed.Type)
		}
	}
	if err := b.signIn(ctx, creds); err != nil {
		return nil, err
	}
	return b, nil
}

// gcpBackend is the secretBackend for store "gcp-secret-manager": GCP Secret Manager under the
// person's own Google sign-in, pinned to settings.GCPProject.
type gcpBackend struct {
	settings Settings
	client   *secretmanager.Client
	creds    *google.Credentials
	// credsKind is the ADC JSON's own "type" (google.AuthorizedUser for a person's own sign-in,
	// google.ServiceAccount for a key file, google.ExternalAccount for a workload identity
	// federation credential such as a dev box's), "" when ADC carries no JSON at all (a GCE/GKE
	// metadata-server identity or GKE workload identity) or JSON with no "type" (an OAuth client
	// secret file).
	credsKind google.CredentialsType
	// login is the person's gcloud login once creds are it (gcloudLogin) in place of the
	// Application Default Credentials - for every form where there are none, and for a write where
	// they are a machine's own (requireWriteSignIn) - and zero while creds are the Application
	// Default Credentials.
	login gcloudAccount
}

// signIn makes creds this backend's sign-in: it dials the Secret Manager client under them
// (gcpClientDial) and closes the client that client replaces.
func (b *gcpBackend) signIn(ctx context.Context, creds *google.Credentials) error {
	client, err := gcpClientDial(ctx, creds)
	if err != nil {
		return fmt.Errorf("create the Secret Manager client: %w", err)
	}
	if b.client != nil {
		b.client.Close()
	}
	b.client, b.creds = client, creds
	return nil
}

// id is the Secret Manager resource name of the secret whose name under the prefix is slug.
func (b *gcpBackend) id(slug string) string { return b.settings.SecretsPrefix + slug }

func (b *gcpBackend) parent() string { return "projects/" + b.settings.GCPProject }

// secretID is id(slug) with the parent's "projects/<project>/secrets/" stripped, the bare id
// CreateSecretRequest.SecretId takes.
func (b *gcpBackend) secretID(slug string) string {
	return strings.TrimPrefix(b.id(slug), b.parent()+"/secrets/")
}

func (b *gcpBackend) displayName(id string) string { return displayName(b.settings.SecretsPrefix, id) }

// requireAccount is a no-op for GCP, unlike AWS's: every call this backend makes already names
// projects/<gcp_project>/... explicitly in the resource, so a wrong-project sign-in gets a real
// PermissionDenied or NotFound from the API itself rather than silently reading another
// project's secrets - AWS's ListSecrets takes no project parameter at all, which is the hazard
// requireAccount guards there. A sign-in's own project (its quota project, set from a credentials
// file's project_id, which a person's sign-in does not carry) is in any case not reliably
// comparable with the broker's gcp_project without a Resource Manager call this CLI does not
// make.
func (b *gcpBackend) requireAccount(context.Context) error { return nil }

// requireWriteSignIn answers, before any write, the signed-in person's lowercased email for
// --owner me, and makes sure the write runs under a person's own Google Account sign-in:
// Application Default Credentials of type "authorized_user", or the person's gcloud login. Where
// the Application Default Credentials are anything else - a service account key file, a workload
// identity federation credential (a dev box's own identity), a workforce identity federation
// sign-in, or a GCE/GKE metadata-server identity - it signs the backend in with the person's
// gcloud login (gcloudLogin), leaving those credentials as they are, unless
// GOOGLE_APPLICATION_CREDENTIALS chose them, a choice it refuses rather than overrides. Under the
// gcloud login it refuses a token Google does not answer as that login's account. It calls the
// userinfo endpoint (gcpUserinfo) only here, not in newGCPBackend: a read never needs the email,
// and a machine identity's token commonly lacks the userinfo scope that call needs.
func (b *gcpBackend) requireWriteSignIn(ctx context.Context) (string, error) {
	if b.login.email == "" && b.credsKind != google.AuthorizedUser {
		found := gcpCredsFound(b.creds, b.credsKind)
		if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
			return "", fmt.Errorf("a secret is written under your own Google sign-in, never a machine's, and GOOGLE_APPLICATION_CREDENTIALS names %s: run this command without GOOGLE_APPLICATION_CREDENTIALS to sign in as yourself", found)
		}
		creds, account, err := gcloudLogin(ctx)
		if err != nil {
			return "", fmt.Errorf("a secret is written under your own Google sign-in, never this machine's (its Application Default Credentials are %s), so a write uses your gcloud login, but %w", found, err)
		}
		if err := b.signIn(ctx, creds); err != nil {
			return "", err
		}
		b.login = account
	}
	email, err := gcpUserinfo(ctx, b.creds)
	if err != nil {
		return "", err
	}
	if email == "" {
		return "", errors.New("your Google sign-in names no email")
	}
	if b.login.email != "" && !strings.EqualFold(email, b.login.email) {
		return "", fmt.Errorf("the token gcloud lent for %s is %s's, as Google answers it, and a secret is written only under the login this command chose: nothing was written", b.login.email, email)
	}
	return strings.ToLower(email), nil
}

// gcpCredsFound names Application Default Credentials that are not a person's own sign-in by what
// they are, never by the file they were read from: the metadata server's identity, the one that
// carries no JSON, or the "type" a credentials file names, if any (an OAuth client secret file
// names none).
func gcpCredsFound(creds *google.Credentials, kind google.CredentialsType) string {
	if len(creds.JSON) == 0 {
		return "the metadata server's identity, this VM's or this pod's"
	}
	if kind == "" {
		return "credentials that name no type"
	}
	return fmt.Sprintf("credentials of type %q", kind)
}

// signInPrefix is the shell assignments a command line a form prints for running again
// (pipeCommand) needs to sign in as this one did, each shell-quoted: GOOGLE_APPLICATION_CREDENTIALS
// when it was set, and CLOUDSDK_CORE_ACCOUNT when it chose the gcloud login, since either may have
// been set for this one command; "" when neither.
func (b *gcpBackend) signInPrefix() string {
	prefix := ""
	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		prefix += "GOOGLE_APPLICATION_CREDENTIALS=" + shellWord(path) + " "
	}
	if b.login.named {
		prefix += "CLOUDSDK_CORE_ACCOUNT=" + shellWord(b.login.email) + " "
	}
	return prefix
}

// hasCurrentVersion reports whether id is served while versions/latest is ENABLED.
func (b *gcpBackend) hasCurrentVersion(ctx context.Context, id string) (bool, error) {
	version, err := b.client.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: id + "/versions/latest"})
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return version.GetState() == secretmanagerpb.SecretVersion_ENABLED, nil
}

// viewOf is secret as a secretView, less its versions, which only show reads.
func (b *gcpBackend) viewOf(ctx context.Context, secret *secretmanagerpb.Secret) (secretView, error) {
	hasValue, err := b.hasCurrentVersion(ctx, secret.GetName())
	if err != nil {
		return secretView{}, err
	}
	view := secretView{
		Name: b.displayName(secret.GetName()), SecretName: secret.GetName(),
		Owner: secret.GetAnnotations()[policy.TagOwner], Tier: secret.GetAnnotations()[policy.TagTier],
		HasValue: hasValue,
	}
	if t := secret.GetCreateTime(); t != nil {
		created := t.AsTime()
		view.Created = &created
	}
	if t := secret.GetExpireTime(); t != nil {
		deleted := t.AsTime()
		view.Deleted = &deleted
	}
	return view, nil
}

func (b *gcpBackend) list(ctx context.Context) ([]secretView, error) {
	views := []secretView{}
	it := b.client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: b.parent()})
	for {
		secret, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list the agent secrets: %w", err)
		}
		if !strings.HasPrefix(secret.GetName(), b.settings.SecretsPrefix) {
			continue // the parent held more than the namespace
		}
		view, err := b.viewOf(ctx, secret)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", secret.GetName(), err)
		}
		views = append(views, view)
	}
	return views, nil
}

// versionsOf maps each of id's version numbers to its one-element state, the GCP counterpart of
// AWS's version-to-stages map.
func (b *gcpBackend) versionsOf(ctx context.Context, id string) (map[string][]string, error) {
	versions := map[string][]string{}
	it := b.client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: id})
	for {
		v, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		number := v.GetName()[strings.LastIndex(v.GetName(), "/")+1:]
		versions[number] = []string{v.GetState().String()}
	}
	return versions, nil
}

func (b *gcpBackend) show(ctx context.Context, slug string) (secretView, error) {
	id := b.id(slug)
	secret, err := b.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: id})
	if err != nil {
		return secretView{}, err
	}
	view, err := b.viewOf(ctx, secret)
	if err != nil {
		return secretView{}, err
	}
	versions, err := b.versionsOf(ctx, id)
	if err != nil {
		return secretView{}, err
	}
	view.Versions = versions
	return view, nil
}

func (b *gcpBackend) create(ctx context.Context, slug, owner, tier, value string) error {
	secret, err := b.client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   b.parent(),
		SecretId: b.secretID(slug),
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{
						CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{
							KmsKeyName: b.settings.KMSKeyName,
						},
					},
				},
			},
			Annotations: map[string]string{policy.TagOwner: owner, policy.TagTier: tier},
		},
	})
	if err != nil {
		return err
	}
	_, err = b.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  secret.GetName(),
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)},
	})
	return err
}

func (b *gcpBackend) set(ctx context.Context, slug, value string) error {
	_, err := b.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  b.id(slug),
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)},
	})
	return err
}

func (b *gcpBackend) currentTags(ctx context.Context, slug string) (owner, tier string, err error) {
	secret, err := b.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: b.id(slug)})
	if err != nil {
		return "", "", err
	}
	return secret.GetAnnotations()[policy.TagOwner], secret.GetAnnotations()[policy.TagTier], nil
}

// retag changes only the owner and tier annotations, preserving any other annotation the secret
// holds: an UpdateSecret whose mask names "annotations" replaces the whole map, unlike AWS's
// TagResource, which touches only the tags it names, so this reads the secret's current
// annotations first and sends the merged map rather than one holding only owner and tier. It
// sends the read's Etag, so a concurrent change to the secret between the read and this write
// (another annotation, say) fails the update with FAILED_PRECONDITION instead of being silently
// overwritten by this request's stale map.
func (b *gcpBackend) retag(ctx context.Context, slug, owner, tier string) error {
	secret, err := b.client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: b.id(slug)})
	if err != nil {
		return err
	}
	annotations := secret.GetAnnotations()
	merged := make(map[string]string, len(annotations)+2)
	for k, v := range annotations {
		merged[k] = v
	}
	merged[policy.TagOwner] = owner
	merged[policy.TagTier] = tier
	_, err = b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:        b.id(slug),
			Annotations: merged,
			Etag:        secret.GetEtag(),
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}},
	})
	return err
}

func (b *gcpBackend) delete(ctx context.Context, slug string) (*time.Time, error) {
	expire := time.Now().UTC().Add(time.Duration(recoveryWindowDays) * 24 * time.Hour)
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:       b.id(slug),
			Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(expire)},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	if err != nil {
		return nil, err
	}
	return &expire, nil
}

// restore clears expire_time: the field mask names it with no value in the body, which Secret
// Manager reads as unsetting it.
func (b *gcpBackend) restore(ctx context.Context, slug string) error {
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: b.id(slug)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}},
	})
	return err
}

func (b *gcpBackend) isAccessDenied(err error) bool {
	return status.Code(err) == codes.PermissionDenied
}

// close releases the Secret Manager client's underlying gRPC connection.
func (b *gcpBackend) close() error { return b.client.Close() }

var _ secretBackend = (*gcpBackend)(nil)
