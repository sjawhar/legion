// Package daemonpermissions names the NATS subjects one Legion daemon uses on Envoy's NATS, and the
// grant of exactly those: the daemon's own consumer names and reply inbox are built here, so the
// daemon that uses them and the grant that admits them cannot drift apart.
package daemonpermissions

import (
	"errors"
	"regexp"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// projectKey is a Legion project key as legion.yaml's `projects` keys are checked
// (packages/daemon/internal/config/stage3.go). Every name below holds the project as part of one
// subject token, so a key with a dot or a wildcard would widen the grant past the project.
var projectKey = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)

// DispatchConsumer is the durable consumer through which project's daemon reads Dispatch's issue
// events from the notification stream, filtered to DispatchFilter.
func DispatchConsumer(project string) string {
	return "legion-go-" + project + "-dispatch"
}

// DispatchFilter is the Dispatch consumer's one filter subject: every Dispatch issue event.
const DispatchFilter = bus.NotificationTopicPrefix + "dispatch.issue.>"

// GitHubConsumer is the durable consumer through which project's daemon reads its repositories'
// GitHub events from the notification stream.
func GitHubConsumer(project string) string {
	return "legion-go-" + project + "-github"
}

// InboxPrefix is the reply inbox prefix of project's daemon connection when it authenticates as the
// daemon's own NATS user (nats.CustomInboxPrefix). It lies outside `_INBOX.`, so no client granted
// `_INBOX.>` receives the daemon's replies, and the daemon's grant receives no other client's.
func InboxPrefix(project string) string {
	return "_INBOX_legion_daemon_" + project
}

// RoleExceptions is the subject the daemon receives role-lane delivery exceptions on: the
// exception lane of every role topic, one token per role.
const RoleExceptions = bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "*"

// Permissions is one NATS user's publish and subscribe allow lists.
type Permissions struct {
	Publish, Subscribe []string
}

// Grant returns the NATS grant of project's daemon, refusing a project that is not a Legion
// project key. Publish: the notification stream's info request, then for each of the project's two
// durable consumers its info request, its create request, its next-message request, and its
// acknowledgements. nats.go appends a consumer's filter subject to the create subject when it has
// exactly one, as the Dispatch consumer does (DispatchFilter), and the server then refuses a
// request whose filter is another, so that create is granted with its filter and no other; a
// consumer of several filters, as the GitHub consumer is (one per repository), sends its bare name.
// Subscribe: the role-lane exceptions, then the project's reply inbox.
//
// Each call owns its lists so a consumer can narrow a grant without changing another caller's.
func Grant(project string) (Permissions, error) {
	if !projectKey.MatchString(project) {
		return Permissions{}, errors.New("the daemon's project must be a Legion project key (" + projectKey.String() + ")")
	}
	const api, ack = "$JS.API.", "$JS.ACK." + bus.Stream + "."
	dispatch, github := bus.Stream+"."+DispatchConsumer(project), bus.Stream+"."+GitHubConsumer(project)
	return Permissions{
		Publish: []string{
			api + "STREAM.INFO." + bus.Stream,
			api + "CONSUMER.INFO." + dispatch,
			api + "CONSUMER.CREATE." + dispatch + "." + DispatchFilter,
			api + "CONSUMER.MSG.NEXT." + dispatch,
			ack + DispatchConsumer(project) + ".>",
			api + "CONSUMER.INFO." + github,
			api + "CONSUMER.CREATE." + github,
			api + "CONSUMER.MSG.NEXT." + github,
			ack + GitHubConsumer(project) + ".>",
		},
		Subscribe: []string{RoleExceptions, InboxPrefix(project) + ".>"},
	}, nil
}
