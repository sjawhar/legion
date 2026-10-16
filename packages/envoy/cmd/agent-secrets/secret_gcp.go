// packages/envoy/cmd/agent-secrets/secret_gcp.go
//
// The GCP side of the secret forms: the Secret Manager calls they make, under the person's own
// Application Default Credentials, and the checks that refuse a read or a write under an identity
// in another project and a write under any identity but a person's own.
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
	"net/http"
	"regexp"
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
)

// gcpKMSKeyNamePattern matches a CMEK key name,
// projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>.
var gcpKMSKeyNamePattern = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)

// checkSettingsKMSKeyName refuses, before any GCP call, settings naming no gcp_project or a
// kms_key_name that is not a CMEK key name: the GCP counterpart of checkSettingsKeyARN.
func checkSettingsKMSKeyName(settings Settings) error {
	if settings.GCPProject == "" {
		return errors.New("the broker's settings name no gcp_project")
	}
	if !gcpKMSKeyNamePattern.MatchString(settings.KMSKeyName) {
		return fmt.Errorf("the broker's settings name an invalid KMS key name %q (expected projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>)", settings.KMSKeyName)
	}
	return nil
}

// gcpIdentityInfo is what the GCP backend knows about the signed-in Application Default
// Credentials: the ADC JSON's own "type" (google.AuthorizedUser for a person's `gcloud auth
// application-default login`, google.ServiceAccount for a key file, "" when ADC carries no JSON at
// all, such as a GCE/GKE metadata-server identity or workload identity), the signed-in email, and
// the project ADC itself names (its quota project), "" when it names none.
type gcpIdentityInfo struct {
	Kind    google.CredentialsType
	Email   string
	Project string
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
// Secret Manager server instead of the real API.
var gcpClientDial = func(ctx context.Context, creds *google.Credentials) (*secretmanager.Client, error) {
	return secretmanager.NewClient(ctx, option.WithCredentials(creds))
}

// newGCPBackend loads the person's Application Default Credentials, reads their identity
// (gcpCredentials, gcpUserinfo) and dials the Secret Manager client (gcpClientDial).
func newGCPBackend(ctx context.Context, settings Settings) (*gcpBackend, error) {
	creds, err := gcpCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("load your Google Cloud Application Default Credentials (run `gcloud auth application-default login`): %w", err)
	}
	identity := gcpIdentityInfo{Project: creds.ProjectID}
	if len(creds.JSON) > 0 {
		var parsed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(creds.JSON, &parsed); err == nil {
			identity.Kind = google.CredentialsType(parsed.Type)
		}
	}
	email, err := gcpUserinfo(ctx, creds)
	if err != nil {
		return nil, err
	}
	identity.Email = strings.ToLower(email)
	client, err := gcpClientDial(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("create the Secret Manager client: %w", err)
	}
	return &gcpBackend{settings: settings, client: client, identity: identity}, nil
}

// gcpBackend is the secretBackend for store "gcp-secret-manager": GCP Secret Manager under the
// person's own Application Default Credentials, pinned to settings.GCPProject.
type gcpBackend struct {
	settings Settings
	client   *secretmanager.Client
	identity gcpIdentityInfo
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

// gcpProjectNumberPattern matches a GCP project NUMBER (digits only), the form the broker's
// gcp_project setting takes (it is cut from the secrets' own resource-name prefix,
// projects/<number>/secrets/<id>).
var gcpProjectNumberPattern = regexp.MustCompile(`^[0-9]+$`)

// requireAccount refuses, before any read, Application Default Credentials naming a project
// NUMBER other than the broker's: a read there would list another project's secrets as if they
// were the agent secrets. ADC's own project (its quota project, when one is set) is ordinarily a
// project ID string, not a number, and resolving one from the other needs a Resource Manager call
// this CLI does not make, so the check only fires when ADC's project is itself numeric and
// disagrees with the broker's; a project ID quota project, or none at all, is silent here (the
// write check, requireWriteSignIn, still refuses a non-person identity regardless).
func (b *gcpBackend) requireAccount(context.Context) error {
	if gcpProjectNumberPattern.MatchString(b.identity.Project) && b.identity.Project != b.settings.GCPProject {
		return fmt.Errorf("your Google Cloud Application Default Credentials are for project %s, but the agent secrets are in project %s: run `gcloud auth application-default login` for the right account, or `gcloud auth application-default set-quota-project %s`",
			b.identity.Project, b.settings.GCPProject, b.settings.GCPProject)
	}
	return nil
}

// requireWriteSignIn refuses, before any write, Application Default Credentials for another
// project (requireAccount) or one that is not a person's own `gcloud auth application-default
// login` (a service account key file, or a GCE/GKE metadata-server or workload identity, whose ADC
// carries no "authorized_user" JSON); it answers the signed-in person's lowercased email for
// --owner me.
func (b *gcpBackend) requireWriteSignIn(ctx context.Context) (string, error) {
	if err := b.requireAccount(ctx); err != nil {
		return "", err
	}
	if b.identity.Kind != google.AuthorizedUser {
		return "", fmt.Errorf("your Google Cloud Application Default Credentials are not a person's sign-in (run `gcloud auth application-default login` instead of using a service account): a secret is written under your own sign-in, never a machine's")
	}
	if b.identity.Email == "" {
		return "", errors.New("your Google sign-in names no email")
	}
	return b.identity.Email, nil
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

func (b *gcpBackend) retag(ctx context.Context, slug, owner, tier string) error {
	_, err := b.client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:        b.id(slug),
			Annotations: map[string]string{policy.TagOwner: owner, policy.TagTier: tier},
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

var _ secretBackend = (*gcpBackend)(nil)
