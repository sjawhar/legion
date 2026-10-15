package core_test

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// listenerEnv runs Check, as a component does before it registers anything, then Env, and returns
// the environment one variable per line.
func listenerEnv(c core.ListenerConfig, urls ...string) (string, error) {
	if err := c.Check(); err != nil {
		return "", err
	}
	env, err := c.Env(urls)
	return strings.Join(env, "\n"), err
}

// Where a value enters core, a value holding an nkey seed (bus.HoldsSeed) is refused naming
// the field by its path, never quoting it, and nothing is rendered: the authorization's names,
// rule entries and noAuthUser, Render's and StartScript's header lines, StartScript's server
// arguments and config path, and the listener's machineId, hostBridge and NATS URLs. Otherwise
// each would reach a refusal's %q, the rendered authorization, the start script or the
// environment a watchdog writes as root. Each row's clean value, in the same place, is accepted and
// reaches that output.
func TestASeedInAnInputIsRefusedUnechoed(t *testing.T) {
	const path = "/tmp/nats-server.conf"
	const url = "nats://127.0.0.1:4222"
	render := func(edit func(*core.Authorization)) (string, error) {
		a := placeholder()
		edit(&a)
		return a.Render(header)
	}
	startScript := func(header, args []string, configPath string) (string, error) {
		return placeholder().StartScript(header, args, configPath)
	}
	listener := core.ListenerConfig{MachineID: "example-host-a", Port: core.DefaultListenerPort}
	// A field no output carries reaches only a refusal, so its clean value is accepted but not
	// rendered: a pending user is never rendered.
	unrendered := map[string]bool{"a pending user's name": true}
	// split cuts v in two DNS labels; joined is text in either case with '.', '-', '_' and
	// whitespace removed, as a reader joins a seed split over them.
	split := func(v string) string { return v[:len(v)/2] + "." + v[len(v)/2:] }
	joined := func(text string) string {
		return strings.Map(func(r rune) rune {
			if strings.ContainsRune(".-_ \t\r\n", r) {
				return -1
			}
			return unicode.ToUpper(r)
		}, text)
	}
	shown := func(text string) bool { return strings.Contains(joined(text), readmeSeed) }
	rows := []struct {
		name  string
		call  func(v string) (string, error)
		want  string // the refusal
		input string // the core.InputError's input, when the refusal is one
		clean string
	}{
		{"a user's name", func(v string) (string, error) {
			return render(func(a *core.Authorization) { a.Users[0].Name = v })
		}, "users[0]: name holds an nkey seed (not echoed)", "", "example-other"},
		{"a pending user's name", func(v string) (string, error) {
			return render(func(a *core.Authorization) { a.Users[3].Name = v })
		}, "users[3]: name holds an nkey seed (not echoed)", "", "example-other"},
		{"a user's name, lowercase", func(v string) (string, error) {
			return render(func(a *core.Authorization) { a.Users[0].Name = strings.ToLower(v) })
		}, "users[0]: name holds an nkey seed (not echoed)", "", "example-other"},
		{"a publish allow entry", func(v string) (string, error) {
			return render(func(a *core.Authorization) { a.Users[0].Publish.Allow = append(a.Users[0].Publish.Allow, "envoy."+v) })
		}, `users[0] "example-listener": publish.allow[1] holds an nkey seed (not echoed)`, "", "other"},
		{"a publish allow entry, lowercase", func(v string) (string, error) {
			return render(func(a *core.Authorization) {
				a.Users[0].Publish.Allow = append(a.Users[0].Publish.Allow, "envoy."+strings.ToLower(v))
			})
		}, `users[0] "example-listener": publish.allow[1] holds an nkey seed (not echoed)`, "", "other"},
		{"a subscribe allow entry, split across subject tokens", func(v string) (string, error) {
			return render(func(a *core.Authorization) {
				a.Users[0].Subscribe.Allow = append(a.Users[0].Subscribe.Allow, "_INBOX."+split(v))
			})
		}, `users[0] "example-listener": subscribe.allow[2] holds an nkey seed (not echoed)`, "", "otherlabel"},
		{"noAuthUser, lowercase", func(v string) (string, error) {
			return render(func(a *core.Authorization) {
				if v != "example-fallback" {
					a.NoAuthUser = strings.ToLower(v)
				}
			})
		}, "noAuthUser holds an nkey seed (not echoed)", "", "example-fallback"},
		{"Render's header line, lowercase", func(v string) (string, error) {
			return placeholder().Render([]string{"fine", "a " + strings.ToLower(v)})
		}, "header[1] holds an nkey seed (not echoed)", "", "clean"},
		{"the listener's hostBridge, split across DNS labels", func(v string) (string, error) {
			c := listener
			c.HostBridge = split(v) + ".example"
			return listenerEnv(c, url)
		}, "hostBridge holds an nkey seed (not echoed)", "hostBridge", "cleanlabels"},
		{"a NATS URL's host, split across DNS labels", func(v string) (string, error) {
			return listenerEnv(listener, url, "nats://"+split(v)+".example:4222")
		}, "natsUrls[1] holds an nkey seed (not echoed)", "natsUrls", "cleanlabels"},
		{"a NATS URL, lowercase", func(v string) (string, error) {
			return listenerEnv(listener, url, "nats://example:"+strings.ToLower(v)+"@hub.internal.example:4222")
		}, "natsUrls[1] holds an nkey seed (not echoed)", "natsUrls", "password"},
		{"a publish deny entry", func(v string) (string, error) {
			return render(func(a *core.Authorization) { a.Users[0].Publish.Deny = append(a.Users[0].Publish.Deny, "envoy."+v) })
		}, `users[0] "example-listener": publish.deny[1] holds an nkey seed (not echoed)`, "", "other"},
		{"a subscribe allow entry", func(v string) (string, error) {
			return render(func(a *core.Authorization) {
				a.Users[0].Subscribe.Allow = append(a.Users[0].Subscribe.Allow, "_INBOX."+v)
			})
		}, `users[0] "example-listener": subscribe.allow[2] holds an nkey seed (not echoed)`, "", "other"},
		{"a subscribe allow entry's queue group", func(v string) (string, error) {
			return render(func(a *core.Authorization) {
				a.Users[1].Subscribe.Allow = append(a.Users[1].Subscribe.Allow, "legion.other "+v)
			})
		}, `users[1] "example-daemon": subscribe.allow[2] holds an nkey seed (not echoed)`, "", "workers"},
		{"a subscribe deny entry", func(v string) (string, error) {
			return render(func(a *core.Authorization) { a.Users[0].Subscribe.Deny = []string{"envoy." + v} })
		}, `users[0] "example-listener": subscribe.deny[0] holds an nkey seed (not echoed)`, "", "other"},
		{"noAuthUser", func(v string) (string, error) {
			return render(func(a *core.Authorization) {
				if v == "example-fallback" {
					return // the clean row: the fallback the placeholder already names
				}
				a.NoAuthUser = v
			})
		}, "noAuthUser holds an nkey seed (not echoed)", "", "example-fallback"},
		{"Render's header line", func(v string) (string, error) {
			return placeholder().Render([]string{"fine", "a " + v})
		}, "header[1] holds an nkey seed (not echoed)", "", "clean"},
		{"StartScript's authorization", func(v string) (string, error) {
			a := placeholder()
			a.Users[0].Name = v
			return a.StartScript(header, nil, path)
		}, "authorization: users[0]: name holds an nkey seed (not echoed)", "authorization", "example-other"},
		{"StartScript's configHeader line", func(v string) (string, error) {
			return startScript([]string{"fine", "a " + v}, nil, path)
		}, "configHeader: header[1] holds an nkey seed (not echoed)", "configHeader", "clean"},
		{"StartScript's serverArgs element", func(v string) (string, error) {
			return startScript(header, []string{"-js", "--server_name=" + v}, path)
		}, "serverArgs[1] holds an nkey seed (not echoed)", "serverArgs", "example"},
		{"StartScript's configPath", func(v string) (string, error) {
			return startScript(header, nil, "/tmp/"+v+"/nats-server.conf")
		}, "configPath holds an nkey seed (not echoed)", "configPath", "example"},
		{"the listener's machineId", func(v string) (string, error) {
			c := listener
			c.MachineID = "host-" + v
			return listenerEnv(c, url)
		}, "machineId holds an nkey seed (not echoed)", "machineId", "example"},
		{"the listener's hostBridge", func(v string) (string, error) {
			c := listener
			c.HostBridge = "bridge-" + v + ".example"
			return listenerEnv(c, url)
		}, "hostBridge holds an nkey seed (not echoed)", "hostBridge", "example"},
		{"a NATS URL", func(v string) (string, error) {
			return listenerEnv(listener, url, "nats://example:"+v+"@hub.internal.example:4222")
		}, "natsUrls[1] holds an nkey seed (not echoed)", "natsUrls", "password"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			out, err := row.call(readmeSeed)
			require.False(t, shown(out), "the seed was rendered")
			if err != nil {
				require.False(t, shown(err.Error()), "the refusal quotes the seed: %q", err)
			}
			require.EqualError(t, err, row.want)
			require.Empty(t, out)
			var refused *core.InputError
			if row.input != "" {
				require.True(t, errors.As(err, &refused), "%v is a core.InputError", err)
				require.Equal(t, row.input, refused.Input)
			}

			out, err = row.call(row.clean)
			require.NoError(t, err, "the clean value is accepted")
			if !unrendered[row.name] {
				require.Contains(t, joined(out), joined(row.clean), "the clean value reaches the output")
			}
		})
	}
}
