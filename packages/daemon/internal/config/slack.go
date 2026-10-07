package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

// Slack is the `slack` block: the workspace of the Slack app this deployment's controller takes
// reports from, and the channels where people report to Legion, each with the Dispatch project a
// report there is filed in. Envoy publishes each mention of the app in a channel on
// `notifications.slack.<team>.<channel>.mention`; the controller subscribes to that topic for each
// reporting channel and nothing else, so a mention anywhere else, or a direct message, reaches no
// controller (skills/legion-controller, "Reports from Slack").
// The daemon tells `legion controller start` the block on the controller secret route, so it carries
// its wire names too.
type Slack struct {
	// Team is the workspace's team id (`T…`), the segment Envoy's Slack topics carry.
	Team string `json:"team"`
	// ReportingChannels are the channel ids (`C…` public, `G…` private), in file order, each with
	// the project a report there is filed in unless it is plainly the work of another project this
	// list names.
	ReportingChannels []ReportingChannel `json:"reportingChannels"`
}

// ReportingChannel is one `slack.reporting_channels` entry.
type ReportingChannel struct {
	Channel string `json:"channel"`
	Project string `json:"project"`
}

var (
	slackTeamPattern    = regexp.MustCompile(`^T[A-Z0-9]+$`)
	slackChannelPattern = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
)

// readSlack reads `slack`: `team` and a non-empty `reporting_channels` mapping of channel id to
// project key. A channel's project need not be one this daemon runs (`projects`): an issue the
// controller files there with the `legion` label in `todo` is admitted by that project's own
// controller.
func readSlack(value *yaml.Node, key string) (*Slack, error) {
	if value.Tag == "!!null" || value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping with team and reporting_channels", key)
	}
	fields, err := members(value, key, "team", "reporting_channels")
	if err != nil {
		return nil, err
	}
	team, err := requiredString(fields["team"], key+".team", ": the Slack workspace's team id (T…), which Envoy's Slack topics carry")
	if err != nil {
		return nil, err
	}
	if !slackTeamPattern.MatchString(team) {
		return nil, fmt.Errorf("%s.team must be a Slack team id such as T0123ABCD (got %q); a workspace name or URL is not one", key, team)
	}
	channels, err := readReportingChannels(fields["reporting_channels"], key+".reporting_channels")
	if err != nil {
		return nil, err
	}
	return &Slack{Team: team, ReportingChannels: channels}, nil
}

func readReportingChannels(value *yaml.Node, key string) ([]ReportingChannel, error) {
	shape := fmt.Errorf("%s must be a mapping of Slack channel id to the Dispatch project its reports are filed in, e.g. {C0123ABCD: ACME}", key)
	if value == nil || value.Kind != yaml.MappingNode {
		return nil, shape
	}
	channels := make([]ReportingChannel, 0, len(value.Content)/2)
	for i := 0; i+1 < len(value.Content); i += 2 {
		channelNode, projectNode := value.Content[i], value.Content[i+1]
		channel := channelNode.Value
		if channelNode.Kind != yaml.ScalarNode || !slackChannelPattern.MatchString(channel) {
			return nil, fmt.Errorf("%s key %q must be a Slack channel id such as C0123ABCD (public) or G0123ABCD (private); a channel name is not one, and a direct message (D…) is never a reporting channel", key, channel)
		}
		if slices.ContainsFunc(channels, func(c ReportingChannel) bool { return c.Channel == channel }) {
			return nil, fmt.Errorf("%s names %s twice", key, channel)
		}
		var project string
		if projectNode.Kind != yaml.ScalarNode || projectNode.Decode(&project) != nil || !projectKeyPattern.MatchString(project) {
			return nil, fmt.Errorf("%s.%s must be a Dispatch project key matching ^[A-Z][A-Z0-9]*$", key, channel)
		}
		channels = append(channels, ReportingChannel{Channel: channel, Project: project})
	}
	if len(channels) == 0 {
		return nil, shape
	}
	return channels, nil
}

// resolveSlack keeps `slack` when the file sets it. The controller files each report as a Dispatch
// issue, so a deployment with Slack reports and no Dispatch is refused here rather than when the
// first report arrives.
func resolveSlack(file fileConfig, cfg *Config) error {
	if file.Slack == nil {
		return nil
	}
	if cfg.DispatchURL == "" {
		return errors.New("slack needs dispatch_url: the controller files each report as a Dispatch issue")
	}
	cfg.Slack = file.Slack
	return nil
}
