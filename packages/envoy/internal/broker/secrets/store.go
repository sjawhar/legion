// Package secrets is the broker's reading of the agent secrets: Store, the one interface the policy
// lists and describes them through and a grant's value is read through, with two stores behind
// it, AWS over AWS Secrets Manager and KMS and GCP over Google Cloud Secret Manager, and Local,
// the in-memory stand-in for the AWS APIs that local development and tests run on.
package secrets

import (
	"context"
	"errors"
	"slices"
)

// BROKER_SECRETS_STORE's values: which store the broker reads the agent secrets from.
const (
	// StoreAWS is AWS Secrets Manager, with the agent-secrets key in AWS KMS.
	StoreAWS = "aws-secrets-manager"
	// StoreGCP is Google Cloud Secret Manager, with the agent-secrets key in Cloud KMS.
	StoreGCP = "gcp-secret-manager"
)

// ErrNotFound is a secret the store does not hold: Describe's answer for a name no secret carries,
// and Read's for a source the store no longer holds or that has no current value.
var ErrNotFound = errors.New("secret not found in the store")

// Store is what the broker reads agent secrets with. List and Describe answer the metadata the
// policy decides from, and Read answers a granted value; nothing here writes a secret.
type Store interface {
	// List answers every secret whose full name starts with prefix, deleted ones left out or
	// marked Deleted, in no promised order. A store that answers more is filtered by the policy.
	List(ctx context.Context, prefix string) ([]Listing, error)
	// Describe answers one secret by its full name, read-after-write consistent where List may
	// lag a change, or ErrNotFound when none exists. A secret scheduled for deletion is answered
	// with Deleted set.
	Describe(ctx context.Context, id string) (Listing, error)
	// Read answers the current value of the secret source names (Listing.Source): ErrNotFound
	// for one the store no longer holds, one scheduled for deletion or one with no current value.
	Read(ctx context.Context, source string) (string, error)
	// ValidName refuses, before any call is made, a full name no secret of this store can carry:
	// one past its length limit, or outside its character set. The error names the rule and never
	// wraps a lookup's failure.
	ValidName(id string) error
}

// Listing is one secret's metadata, the same shape from List and Describe.
type Listing struct {
	// ID is the secret's full name under the prefix: its Secrets Manager name, or its Secret
	// Manager resource name (projects/<number>/secrets/<id>).
	ID string
	// Source is what Read takes: the secret's ARN, or its Secret Manager resource name.
	Source string
	// Tags carry the policy's owner and tier: the secret's AWS tags, or its Secret Manager
	// annotations.
	Tags map[string]string
	// OnKey is whether the secret is encrypted with the agent-secrets key the store was built
	// with. A secret on the provider's default key is not.
	OnKey bool
	// HasCurrentValue is whether a read would answer a value: a version labelled AWSCURRENT, or a
	// Secret Manager versions/latest that is ENABLED.
	HasCurrentValue bool
	// Deleted is a secret scheduled for deletion: Secrets Manager's DeletedDate, or Secret
	// Manager's expire_time.
	Deleted bool
}

// currentStage is the staging label GetSecretValue reads when it names no version.
const currentStage = "AWSCURRENT"

// HasCurrentVersion reports whether a secret's version stages (ListSecrets'
// SecretVersionsToStages, DescribeSecret's VersionIdsToStages) name a version labelled AWSCURRENT,
// the one GetSecretValue reads: the broker serves a secret only then, and the agent-secrets CLI
// shows it as having a value only then.
func HasCurrentVersion(versionsToStages map[string][]string) bool {
	for _, stages := range versionsToStages {
		if slices.Contains(stages, currentStage) {
			return true
		}
	}
	return false
}
