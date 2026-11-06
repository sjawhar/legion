package requests

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

func TestAzureLoginReleaseIsDisabledWithoutConfiguredSecret(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := policytest.KindSecret("AZURE_LOGIN", fixtureOperator, policy.TierHuman, policy.KindAzureLogin, `{"tenant_id":"tenant","client_id":"client"}`)
	local := secrets.NewLocal(stored)
	set, err := policytest.Loader(local).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	machine := &Machine{
		Secrets: policytest.Store(local),
		Minters: map[string]Minter{policy.KindAzureLogin: (secrets.AzureLogin{TokenFile: file}).Mint},
	}
	secret := set.Secrets["AZURE_LOGIN"]
	value, err := machine.release(t.Context(), requestedSecret{name: secret.Name, source: secret.Source, decision: policy.Approval}, secret, nil)
	if value != "" || !errors.Is(err, ErrMintFailed) || secrets.MintErrorClass(err) != "refused" {
		t.Fatalf("release returned class %s, value present %t; want refused mint and no token", secrets.MintErrorClass(err), value != "")
	}
}

func TestAzureLoginReleaseUsesThePolicySecretNameAndTier(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, secret, tier, owner, decision string
		retagServiceGrant, allowed          bool
	}{
		{"another human secret", "OTHER_LOGIN", policy.TierHuman, policy.OwnerShared, policy.Approval, false, false},
		{"shared agent secret", "OTHER_LOGIN", policy.TierAgent, policy.OwnerShared, policy.Automatic, false, false},
		{"configured agent secret", "AZURE_LOGIN", policy.TierAgent, otherPerson, policy.Approval, false, false},
		{"automatic service grant after retag", "AZURE_LOGIN", policy.TierHuman, policy.OwnerShared, policy.Automatic, true, false},
		{"configured human secret", "AZURE_LOGIN", policy.TierHuman, policy.OwnerShared, policy.Approval, false, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			stored := policytest.KindSecret(row.secret, row.owner, row.tier, policy.KindAzureLogin,
				`{"tenant_id":"tenant","client_id":"client","name":"AZURE_LOGIN","human_tier":true}`)
			if row.retagServiceGrant {
				stored.Tags[policy.TagOwner] = fixtureService
				stored.Tags[policy.TagTier] = policy.TierAgent
			}
			local := secrets.NewLocal(stored)
			set, err := policytest.Loader(local, fixtureService).Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			secret, ok := set.Secrets[row.secret]
			if !ok {
				t.Fatal("policy did not serve the Azure login fixture")
			}
			decision, err := set.Evaluate(row.secret, policy.Requester{Operator: fixtureOperator, Service: fixtureService})
			if err != nil || decision.Outcome != row.decision {
				t.Fatalf("policy decision = %+v, %v, want %s", decision, err, row.decision)
			}
			granted := requestedSecret{name: secret.Name, source: secret.Source, decision: decision.Outcome}
			if row.retagServiceGrant {
				stored.Tags[policy.TagOwner] = row.owner
				stored.Tags[policy.TagTier] = row.tier
				local.Put(stored)
				current, err := policytest.Loader(local, fixtureService).Load(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				secret, ok = current.Secrets[row.secret]
				if !ok || secret.Tier != policy.TierHuman {
					t.Fatal("policy did not serve the retagged secret as human tier")
				}
			}
			machine := &Machine{
				Secrets: policytest.Store(local),
				Minters: map[string]Minter{policy.KindAzureLogin: (secrets.AzureLogin{TokenFile: file, SecretName: "AZURE_LOGIN"}).Mint},
			}
			value, err := machine.release(t.Context(), granted, secret, nil)
			if row.allowed {
				var got map[string]string
				if err != nil || json.Unmarshal([]byte(value), &got) != nil || got["federated_token"] != token {
					t.Fatal("release of the configured human-tier secret did not return its token")
				}
			} else if value != "" || !errors.Is(err, ErrMintFailed) || secrets.MintErrorClass(err) != "refused" {
				t.Fatalf("release returned class %s, value present %t; want refused mint and no token", secrets.MintErrorClass(err), value != "")
			}
		})
	}
}
