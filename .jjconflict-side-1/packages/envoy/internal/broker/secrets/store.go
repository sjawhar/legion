// Package secrets is the broker's reading of the agent secrets: Store, the one interface the policy
// lists and describes them through and a grant's value is read through, with two stores behind
// it, AWS over AWS Secrets Manager and KMS and GCP over Google Cloud Secret Manager, and Local,
// the in-memory stand-in for the AWS APIs that local development and tests run on.
package secrets

import (
	"context"
	"errors"
	"time"
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

// Reader answers a granted secret's value: what releasing a grant needs, and nothing more.
type Reader interface {
	// Read answers the current value of the secret source names (Listing.Source): ErrNotFound
	// for one the store no longer holds, one scheduled for deletion or one with no current value.
	Read(ctx context.Context, source string) (string, error)
}

// Store is what the broker reads agent secrets with. List and Describe answer the metadata the
// policy decides from, and Read answers a granted value; nothing here writes a secret.
type Store interface {
	Reader
	// List answers every secret whose full name starts with prefix, in no promised order. A secret
	// scheduled for deletion is one the store does not hold: List leaves it out, Describe and Read
	// answer ErrNotFound for it. A store that answers more is filtered by the policy.
	List(ctx context.Context, prefix string) ([]Listing, error)
	// Describe answers one secret by its full name, or ErrNotFound when none exists or it is
	// scheduled for deletion.
	Describe(ctx context.Context, id string) (Listing, error)
	// ValidName refuses, before any call is made, a full name no secret of this store can carry:
	// one past its length limit, or outside its character set. The error names the rule and never
	// wraps a lookup's failure.
	ValidName(id string) error
	// ListLag is how long the store documents its metadata reads may answer a write's previous
	// state: the policy keeps what a single-name read settled, and re-describes it on every
	// reload, for that long.
	ListLag() time.Duration
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
	// OnKey answers whether the secret is encrypted with the agent-secrets key the store was
	// built with; a secret on the provider's default key is not. It may make a call (the AWS
	// store resolves the key's aliases, at most once per List or Describe), so the policy asks it
	// only after every tag check, and a failed call fails the whole load.
	OnKey func(ctx context.Context) (bool, error)
	// HasCurrentValue is whether a read would answer a value: a version labelled AWSCURRENT, or a
	// Secret Manager versions/latest that is ENABLED.
	HasCurrentValue bool
}
