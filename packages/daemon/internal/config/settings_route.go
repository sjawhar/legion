package config

import (
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// settingsRouteKey is `settings_route`, the key every refusal of it names.
const settingsRouteKey = "settings_route"

// serviceAccountPrefix is how Kubernetes writes a ServiceAccount as a token subject.
const serviceAccountPrefix = "system:serviceaccount:"

// SettingsRoute is `settings_route`: who, besides the operator bearer, may change this daemon's
// settings through `PUT /legion/v1/operator/settings`. A deployment that configures none leaves
// the route to the operator bearer alone.
//
// The three are one rule and are configured together: the cluster's OIDC issuer whose signature a
// presented token must carry, the audience that token must have been projected for, and the
// ServiceAccounts it may name. Dispatch's pod projects such a token with the daemon as its
// audience and sends it as its bearer, so Dispatch holds no copy of the operator token; the
// daemon checks the signature against the issuer, as the secrets broker checks its own tokens
// (packages/envoy/oidc).
type SettingsRoute struct {
	// OIDCIssuer is the cluster's OIDC issuer, as the `iss` claim of its projected tokens writes
	// it; "" when the deployment configures no such caller.
	OIDCIssuer string
	// Audience is the audience a presented token must have been projected for.
	Audience string
	// ServiceAccounts is every ServiceAccount the route admits, each
	// `system:serviceaccount:<namespace>:<name>`, which is the token's `sub`. A token naming any
	// other subject is refused.
	ServiceAccounts []string
}

// Configured is whether the deployment named a caller for the settings route at all. The loader
// refuses a half-written block, so a configured block carries all three.
func (s SettingsRoute) Configured() bool {
	return s.OIDCIssuer != "" && s.Audience != "" && len(s.ServiceAccounts) > 0
}

// Admits is whether subject, the `sub` of a verified token, is one of the ServiceAccounts this
// deployment named.
func (s SettingsRoute) Admits(subject string) bool {
	for _, account := range s.ServiceAccounts {
		if account == subject {
			return true
		}
	}
	return false
}

// readSettingsRoute is `settings_route`: a mapping of `oidc_issuer`, `audience` and
// `service_accounts`. An absent key and a null one configure no caller. A block that carries one
// of the three carries all of them: an issuer with no ServiceAccounts would admit every account
// of the cluster, and ServiceAccounts with no issuer would admit a token nobody checked, so a
// half-written block is refused rather than quietly serving the wrong rule.
func readSettingsRoute(value *yaml.Node, key string) (*SettingsRoute, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	found, err := members(value, key, "oidc_issuer", "audience", "service_accounts")
	if err != nil {
		return nil, err
	}
	route := &SettingsRoute{}
	if route.OIDCIssuer, err = optionalString(found["oidc_issuer"], key+".oidc_issuer"); err != nil {
		return nil, err
	}
	if route.Audience, err = optionalString(found["audience"], key+".audience"); err != nil {
		return nil, err
	}
	if accounts := found["service_accounts"]; accounts != nil {
		if accounts.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("%s.service_accounts must be an array", key)
		}
		for index, entry := range accounts.Content {
			account, err := requiredString(entry, fmt.Sprintf("%s.service_accounts[%d]", key, index), "")
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(account, serviceAccountPrefix) || strings.Count(account, ":") != 3 {
				return nil, fmt.Errorf("%s.service_accounts[%d] must be a Kubernetes ServiceAccount subject, %s<namespace>:<name>, and is %q",
					key, index, serviceAccountPrefix, account)
			}
			route.ServiceAccounts = append(route.ServiceAccounts, account)
		}
	}
	if route.OIDCIssuer == "" && route.Audience == "" && len(route.ServiceAccounts) == 0 {
		return nil, nil
	}
	for name, set := range map[string]bool{
		"oidc_issuer":      route.OIDCIssuer != "",
		"audience":         route.Audience != "",
		"service_accounts": len(route.ServiceAccounts) > 0,
	} {
		if !set {
			return nil, fmt.Errorf("%s.%s is required: the issuer, the audience and the ServiceAccounts are one admission rule, and a daemon that read only part of it would admit a caller the deployment did not name", key, name)
		}
	}
	issuer, err := url.Parse(route.OIDCIssuer)
	if err != nil || issuer.Scheme == "" || issuer.Host == "" {
		return nil, fmt.Errorf("%s.oidc_issuer must be the cluster's OIDC issuer URL, as its tokens' iss claim writes it, and is %q", key, route.OIDCIssuer)
	}
	return route, nil
}
