// packages/envoy/cmd/agent-secrets/secret_gcp.go
//
// The GCP side of the secret forms: the Secret Manager calls they make, under the person's own
// Application Default Credentials, and the check that refuses a write under any identity but a
// person's own (GCP, unlike AWS, needs no separate wrong-project read check: every call already
// names the project explicitly, so the API itself refuses one in another project).
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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// newGCPBackend loads the person's Application Default Credentials, reads the ADC JSON's own
// "type" and dials the Secret Manager client (gcpClientDial). It makes no userinfo call: that is
// only needed for a write's --owner me, so requireWriteSignIn makes it lazily, and a read under an
// identity whose token lacks userinfo scope (common for a machine's) still succeeds.
func newGCPBackend(ctx context.Context, settings Settings) (*gcpBackend, error) {
	creds, err := gcpCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("load your Google Cloud Application Default Credentials (run `gcloud auth application-default login`): %w", err)
	}
	var credsKind google.CredentialsType
	if len(creds.JSON) > 0 {
		var parsed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(creds.JSON, &parsed); err == nil {
			credsKind = google.CredentialsType(parsed.Type)
		}
	}
	client, err := gcpClientDial(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("create the Secret Manager client: %w", err)
	}
	return &gcpBackend{settings: settings, client: client, creds: creds, credsKind: credsKind}, nil
}

// gcpBackend is the secretBackend for store "gcp-secret-manager": GCP Secret Manager under the
// person's own Application Default Credentials, pinned to settings.GCPProject.
type gcpBackend struct {
	settings Settings
	client   *secretmanager.Client
	creds    *google.Credentials
	// credsKind is the ADC JSON's own "type" (google.AuthorizedUser for a person's `gcloud auth
	// application-default login`, google.ServiceAccount for a key file, "" when ADC carries no
	// JSON at all, such as a GCE/GKE metadata-server identity or workload identity).
	credsKind google.CredentialsType
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
// projects/<gcp_project>/... explicitly in the resource, so a wrong-project Application Default
// Credentials identity gets a real PermissionDenied or NotFound from the API itself rather than
// silently reading another project's secrets - AWS's ListSecrets takes no project parameter at
// all, which is the hazard requireAccount guards there. ADC's own project (its quota project, set
// from the credentials file's project_id, which an `authorized_user` file from `gcloud auth
// application-default login` does not carry) is in any case not reliably comparable with the
// broker's gcp_project without a Resource Manager call this CLI does not make.
func (b *gcpBackend) requireAccount(context.Context) error { return nil }

// requireWriteSignIn refuses, before any write, Application Default Credentials that are not a
// person's own `gcloud auth application-default login` (a service account key file, or a GCE/GKE
// metadata-server or workload identity, whose ADC carries no "authorized_user" JSON); it answers
// the signed-in person's lowercased email for --owner me. It calls the userinfo endpoint
// (gcpUserinfo) only here, after the Kind check, not in newGCPBackend: a read never needs the
// email, and a machine identity's token commonly lacks the userinfo scope that call needs.
func (b *gcpBackend) requireWriteSignIn(ctx context.Context) (string, error) {
	if b.credsKind != google.AuthorizedUser {
		return "", fmt.Errorf("your Google Cloud Application Default Credentials are not a person's sign-in (run `gcloud auth application-default login` instead of using a service account): a secret is written under your own sign-in, never a machine's")
	}
	email, err := gcpUserinfo(ctx, b.creds)
	if err != nil {
		return "", err
	}
	if email == "" {
		return "", errors.New("your Google sign-in names no email")
	}
	return strings.ToLower(email), nil
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
