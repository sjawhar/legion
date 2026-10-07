package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const validSlack = `
  team: T0WORKSPACE
  reporting_channels:
    C0REPORTS: ACME
    G0PRIVATE: OTHER
`

// A Slack report may belong to a project this daemon does not run: that deployment's controller
// admits it. The config preserves each mapping entry in file order for the controller's prompt.
func TestLoadReadsSlackReportingChannels(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"slack:"+validSlack), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	want := &Slack{Team: "T0WORKSPACE", ReportingChannels: []ReportingChannel{
		{Channel: "C0REPORTS", Project: "ACME"},
		{Channel: "G0PRIVATE", Project: "OTHER"},
	}}
	if !reflect.DeepEqual(cfg.Slack, want) {
		t.Errorf("Slack = %#v, want %#v", cfg.Slack, want)
	}
}

// Every malformed shape readSlack and readReportingChannels refuse is pinned here. A Slack
// reporting channel is deliberately only a public or private channel ID: a direct-message ID
// cannot be subscribed to without also receiving every ordinary message in the bot's channels.
func TestLoadRefusesEveryInvalidSlackConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, slack, want string
	}{
		{
			name: "a null Slack block", slack: " null\n",
			want: "slack must be a mapping with team and reporting_channels",
		},
		{
			name: "a scalar Slack block", slack: " configured\n",
			want: "slack must be a mapping with team and reporting_channels",
		},
		{
			name: "an unknown Slack member", slack: `
  team: T0WORKSPACE
  reporting_channels: {C0REPORTS: ACME}
  elsewhere: nope
`, want: "unknown key slack.elsewhere",
		},
		{
			name: "a duplicate Slack member", slack: `
  team: T0WORKSPACE
  team: T0SECOND
  reporting_channels: {C0REPORTS: ACME}
`, want: "slack names team twice",
		},
		{
			name: "a missing team", slack: `
  reporting_channels: {C0REPORTS: ACME}
`, want: "slack.team is required",
		},
		{
			name: "a null team", slack: `
  team:
  reporting_channels: {C0REPORTS: ACME}
`, want: "slack.team is required",
		},
		{
			name: "an empty team", slack: `
  team: ""
  reporting_channels: {C0REPORTS: ACME}
`, want: "slack.team must not be empty",
		},
		{
			name: "a non-string team", slack: `
  team: [T0WORKSPACE]
  reporting_channels: {C0REPORTS: ACME}
`, want: "slack.team must be a string",
		},
		{
			name: "a non-ID team", slack: `
  team: workspace-name
  reporting_channels: {C0REPORTS: ACME}
`, want: "slack.team must be a Slack team id",
		},
		{
			name: "missing reporting channels", slack: `
  team: T0WORKSPACE
`, want: "slack.reporting_channels must be a mapping",
		},
		{
			name: "null reporting channels", slack: `
  team: T0WORKSPACE
  reporting_channels:
`, want: "slack.reporting_channels must be a mapping",
		},
		{
			name: "a scalar reporting channels value", slack: `
  team: T0WORKSPACE
  reporting_channels: C0REPORTS
`, want: "slack.reporting_channels must be a mapping",
		},
		{
			name: "empty reporting channels", slack: `
  team: T0WORKSPACE
  reporting_channels: {}
`, want: "slack.reporting_channels must be a mapping",
		},
		{
			name: "a channel name instead of ID", slack: `
  team: T0WORKSPACE
  reporting_channels: {reports: ACME}
`, want: "a channel name is not one",
		},
		{
			name: "a direct-message channel", slack: `
  team: T0WORKSPACE
  reporting_channels: {D0DIRECT: ACME}
`, want: "a direct message (D…) is never a reporting channel",
		},
		{
			name: "a duplicate channel", slack: `
  team: T0WORKSPACE
  reporting_channels:
    C0REPORTS: ACME
    C0REPORTS: OTHER
`, want: "slack.reporting_channels names C0REPORTS twice",
		},
		{
			name: "a lower-case project", slack: `
  team: T0WORKSPACE
  reporting_channels: {C0REPORTS: acme}
`, want: "slack.reporting_channels.C0REPORTS must be a Dispatch project key",
		},
		{
			name: "a non-string project", slack: `
  team: T0WORKSPACE
  reporting_channels: {C0REPORTS: [ACME]}
`, want: "slack.reporting_channels.C0REPORTS must be a Dispatch project key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"slack:"+tc.slack), noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadSlackReportingChannelsRefusesANonScalarChannelID(t *testing.T) {
	_, err := readReportingChannels(&yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.SequenceNode},
			{Kind: yaml.ScalarNode, Value: "ACME"},
		},
	}, "slack.reporting_channels")
	if err == nil || !strings.Contains(err.Error(), "must be a Slack channel id") {
		t.Fatalf("readReportingChannels error = %v", err)
	}
}

func TestLoadRefusesSlackWithoutDispatch(t *testing.T) {
	withoutDispatch := strings.Replace(minimalFile, "dispatch_url: http://127.0.0.1:8080\n", "", 1)
	_, err := LoadForValidation(writeConfigFile(t, withoutDispatch+"slack:"+validSlack), noEnv)
	if err == nil || err.Error() != "slack needs dispatch_url: the controller files each report as a Dispatch issue" {
		t.Fatalf("LoadForValidation error = %v", err)
	}
}
