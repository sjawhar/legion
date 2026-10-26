package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// The answers block's pieces a table row assembles: one provider of each credential source, a
// price for each provider's model, and the two database stores' runtime.kubernetes lines.
const (
	anthropicByFile    = "    anthropic: {base_url: \"https://gateway.internal.example/anthropic/\", api_key_file: keys/anthropic}\n"
	openaiByCommand    = "    openai: {base_url: https://gateway.internal.example/openai, api_key_command: printf openai-key}\n"
	opusPrice          = "    anthropic/claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}\n"
	gptPrice           = "    openai/gpt-6.1: {input: 1.25, output: 10, cache_write: 0, cache_read: 0.125}\n"
	postgresStore      = "    session_store: postgres\n    session_dsn_secret: SESSION_DSN\n"
	cloudSQLStore      = "    session_store: cloudsql-iam\n    session_database: {instance: example-project:us-central1:example-instance, name: legion_sessions, user: legion-sessions@example-project.iam}\n"
	daemonDSNFile      = "  conversation_store: {dsn_file: conversations-dsn}\n"
	daemonIAMLogin     = "  conversation_store: {user: legion-daemon@example-project.iam}\n"
	answersRequiredDSN = "answers.conversation_store.dsn_file is required with runtime.kubernetes.session_store postgres: the file holding the postgres:// URL of the database the conversations live in, which the daemon reads them through"
)

// answersBlock is an `answers` block of providers, prices and whatever else rest adds.
func answersBlock(providers, prices, rest string) string {
	return "answers:\n  providers:\n" + providers + "  prices:\n" + prices + rest
}

// No `answers`, or `answers: null`, leaves the loop off under either runtime, the default pvc
// store included: the daemon runs as it did before the block existed.
func TestNoAnswersBlockLeavesTheLoopOff(t *testing.T) {
	for name, body := range map[string]string{
		"tmux, absent":         minimalFile,
		"tmux, null":           minimalFile + "answers: null\n",
		"kubernetes, pvc":      kubernetesFile,
		"kubernetes, null":     "answers:\n" + kubernetesFile,
		"kubernetes, postgres": kubernetesFile + postgresStore,
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, body), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.Answers != nil {
				t.Errorf("Answers = %+v, want nil: no block, no loop", cfg.Answers)
			}
		})
	}
}

// Both providers settle, one keyed by a file resolved against the file's directory and one by a
// command kept as written, with their base URLs' trailing slashes dropped; every price is read;
// the bounds a file leaves out are the defaults; and under tmux the daemon needs no connection to
// the conversations, which are files on its own host.
func TestAnswersSettlesBothProvidersTheirPricesAndTheDefaultBounds(t *testing.T) {
	path := writeConfigFile(t, minimalFile+answersBlock(anthropicByFile+openaiByCommand, opusPrice+gptPrice, ""))
	cfg, err := LoadForValidation(path, noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.Answers == nil {
		t.Fatal("Answers = nil, want the block settled")
	}
	want := Answers{
		Providers: map[AnswerProvider]AnswerEndpoint{
			AnswerAnthropic: {BaseURL: "https://gateway.internal.example/anthropic", Credential: AnswerCredential{
				File: filepath.Join(filepath.Dir(path), "keys/anthropic"), key: "answers.providers.anthropic"}},
			AnswerOpenAI: {BaseURL: "https://gateway.internal.example/openai", Credential: AnswerCredential{
				Command: "printf openai-key", key: "answers.providers.openai"}},
		},
		Prices: map[string]AnswerPrice{
			"anthropic/claude-opus-5-5": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5},
			"openai/gpt-6.1":            {Input: 1.25, Output: 10, CacheWrite: 0, CacheRead: 0.125},
		},
		Bounds: AnswerBounds{
			Answer: 180 * time.Second, ModelCalls: 8, OutputTokens: 4000, GHCall: 30 * time.Second,
			GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 50,
		},
	}
	if !reflect.DeepEqual(*cfg.Answers, want) {
		t.Errorf("Answers = %+v\nwant %+v", *cfg.Answers, want)
	}
}

// Each bound the file sets is the file's, a decimal daily limit included, and each it leaves out
// keeps its default.
func TestAnswersBoundsTheFileSetsOverrideTheDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, bounds string
		want         AnswerBounds
	}{
		{
			"every bound",
			"  bounds: {answer_seconds: 60, model_calls: 1, output_tokens: 16, gh_seconds: 5, gh_output_chars: 100, gh_calls: 2, concurrent: 1, daily_usd: 0.01}\n",
			AnswerBounds{Answer: time.Minute, ModelCalls: 1, OutputTokens: 16, GHCall: 5 * time.Second, GHOutputChars: 100, GHCalls: 2, Concurrent: 1, DailyUSD: 0.01},
		},
		{
			"two bounds",
			"  bounds: {model_calls: 3, daily_usd: 75}\n",
			AnswerBounds{Answer: 180 * time.Second, ModelCalls: 3, OutputTokens: 4000, GHCall: 30 * time.Second, GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 75},
		},
		{
			"an empty mapping",
			"  bounds: {}\n",
			defaultAnswerBounds(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+answersBlock(anthropicByFile, opusPrice, tc.bounds)), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.Answers.Bounds != tc.want {
				t.Errorf("Bounds = %+v, want %+v", cfg.Answers.Bounds, tc.want)
			}
		})
	}
}

// conversation_store is the key the runtime's own store needs, and only that one: dsn_file under
// session_store postgres, user under cloudsql-iam (with session_database's instance and database),
// neither under tmux; and under pvc, the default, `answers` is refused whole.
func TestAnswersConversationStoreFollowsTheRuntimesStore(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       ConversationStore // with dsn_file relative to the file's directory
		refusal    string
	}{
		{
			name: "postgres, dsn_file",
			body: kubernetesFile + postgresStore + answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			want: ConversationStore{DSNFile: "conversations-dsn"},
		},
		{
			name:    "postgres, no conversation_store",
			body:    kubernetesFile + postgresStore + answersBlock(anthropicByFile, opusPrice, ""),
			refusal: answersRequiredDSN,
		},
		{
			name:    "postgres, user alone",
			body:    kubernetesFile + postgresStore + answersBlock(anthropicByFile, opusPrice, daemonIAMLogin),
			refusal: "answers.conversation_store.user is not used when runtime.kubernetes.session_store is postgres; remove it",
		},
		{
			name: "cloudsql-iam, user",
			body: kubernetesFile + cloudSQLStore + answersBlock(anthropicByFile, opusPrice, daemonIAMLogin),
			want: ConversationStore{Database: ompsessions.Database{
				Instance: "example-project:us-central1:example-instance", Name: "legion_sessions", User: "legion-daemon@example-project.iam",
			}},
		},
		{
			name:    "cloudsql-iam, no conversation_store",
			body:    kubernetesFile + cloudSQLStore + answersBlock(anthropicByFile, opusPrice, ""),
			refusal: "answers.conversation_store.user is required with runtime.kubernetes.session_store cloudsql-iam: the IAM login the daemon signs in to runtime.kubernetes.session_database's instance as",
		},
		{
			name:    "cloudsql-iam, dsn_file",
			body:    kubernetesFile + cloudSQLStore + answersBlock(anthropicByFile, opusPrice, "  conversation_store: {dsn_file: conversations-dsn, user: legion-daemon@example-project.iam}\n"),
			refusal: "answers.conversation_store.dsn_file is not used when runtime.kubernetes.session_store is cloudsql-iam; remove it",
		},
		{
			name:    "pvc by default",
			body:    kubernetesFile + answersBlock(anthropicByFile, opusPrice, ""),
			refusal: "answers is not used when runtime.kubernetes.session_store is pvc: each conversation is a file on its issue's volume, which the daemon does not mount and the tree's close deletes, so there is none to answer from; remove answers, or keep conversations in a database (session_store postgres or cloudsql-iam)",
		},
		{
			name:    "pvc named, with a conversation_store",
			body:    kubernetesFile + "    session_store: pvc\n" + answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			refusal: "answers is not used when runtime.kubernetes.session_store is pvc: each conversation is a file on its issue's volume, which the daemon does not mount and the tree's close deletes, so there is none to answer from; remove answers, or keep conversations in a database (session_store postgres or cloudsql-iam)",
		},
		{
			name: "tmux, no conversation_store",
			body: minimalFile + answersBlock(anthropicByFile, opusPrice, ""),
		},
		{
			name:    "tmux, a conversation_store",
			body:    minimalFile + answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			refusal: "answers.conversation_store is not used under runtime tmux: every conversation is a file on the daemon's own host, which it reads where the pane wrote it; remove it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.body)
			cfg, err := LoadForValidation(path, noEnv)
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("LoadForValidation = %v, want %q", err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			want := tc.want
			if want.DSNFile != "" {
				want.DSNFile = filepath.Join(filepath.Dir(path), want.DSNFile)
			}
			if cfg.Answers.ConversationStore != want {
				t.Errorf("ConversationStore = %+v, want %+v", cfg.Answers.ConversationStore, want)
			}
		})
	}
}

// Every shape the block may not take is refused naming the key, an unknown key by its full dotted
// name.
func TestAnswersRefusesEachMalformedShapeByName(t *testing.T) {
	for _, tc := range []struct{ name, block, want string }{
		{"not a mapping", "answers: on\n", "answers must be a mapping"},
		{"an unknown key", answersBlock(anthropicByFile, opusPrice, "  model: anthropic/claude-opus-5-5\n"), "unknown key answers.model"},
		{"no providers", "answers:\n  prices:\n" + opusPrice, "answers.providers is required: each model provider the loop may call (anthropic, openai), its base_url and its key"},
		{"no provider named", answersBlock("    anthropic: null\n", opusPrice, ""), "answers.providers must name at least one provider: anthropic or openai"},
		{"providers not a mapping", "answers:\n  providers: [anthropic]\n  prices:\n" + opusPrice, "answers.providers must be a mapping of provider (anthropic, openai) to its base_url and key"},
		{"an unknown provider", answersBlock(anthropicByFile+"    gemini: {base_url: https://gateway.internal.example/gemini, api_key_file: gemini}\n", opusPrice, ""), "unknown key answers.providers.gemini"},
		{"an unknown provider key", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key: sk-fixture}\n", opusPrice, ""), "unknown key answers.providers.anthropic.api_key"},
		{"both credentials", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key_file: anthropic-key, api_key_command: cat anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic requires exactly one of api_key_file or api_key_command"},
		{"no credential", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic}\n", opusPrice, ""), "answers.providers.anthropic requires exactly one of api_key_file or api_key_command"},
		{"a blank credential", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key_command: \" \"}\n", opusPrice, ""), "answers.providers.anthropic.api_key_command must not be empty"},
		{"no base_url", answersBlock("    anthropic: {api_key_file: anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic.base_url is required: the provider's API endpoint, an http or https URL"},
		{"a relative base_url", answersBlock("    anthropic: {base_url: gateway/anthropic, api_key_file: anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic.base_url must be a valid URL"},
		{"a base_url of another scheme", answersBlock("    anthropic: {base_url: \"ftp://gateway.internal.example/anthropic\", api_key_file: anthropic-key}\n", opusPrice, ""), `answers.providers.anthropic.base_url must be http or https, not "ftp"`},
		{"a base_url with a query", answersBlock("    anthropic: {base_url: \"https://gateway.internal.example/anthropic?key=x\", api_key_file: anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic.base_url must not include a query string or fragment"},
		{"no prices", "answers:\n  providers:\n" + anthropicByFile, "answers.prices is required: the USD price per million tokens of each model the loop may answer on, by the id its conversation names (<provider>/<model>)"},
		{"no price", "answers:\n  providers:\n" + anthropicByFile + "  prices: {}\n", "answers.prices must price at least one model"},
		{"a model id with no provider", answersBlock(anthropicByFile, "    claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), `answers.prices names "claude-opus-5-5", which is no model id: <provider>/<model>, its provider anthropic or openai`},
		{"a model of an unknown provider", answersBlock(anthropicByFile, "    gemini/gemini-3: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), `answers.prices names "gemini/gemini-3", which is no model id: <provider>/<model>, its provider anthropic or openai`},
		{"a model of a provider not configured", answersBlock(anthropicByFile, opusPrice+gptPrice, ""), "answers.prices.openai/gpt-6.1 prices a model of openai, which answers.providers does not name, so the loop never calls it; remove it or configure the provider"},
		{"a model priced twice", answersBlock(anthropicByFile, opusPrice+opusPrice, ""), "answers.prices names anthropic/claude-opus-5-5 twice"},
		{"a price field missing", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25}\n", ""), "answers.prices.anthropic/claude-opus-5-5.cache_read is required: USD per million tokens, 0 when the provider charges nothing for it"},
		{"a negative price", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: -5, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), "answers.prices.anthropic/claude-opus-5-5.input must be a non-negative number"},
		{"a price that is no number", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: five, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), "answers.prices.anthropic/claude-opus-5-5.input must be a number"},
		{"an infinite price", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: .inf, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), "answers.prices.anthropic/claude-opus-5-5.input must be a number"},
		{"an unknown price key", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5, cache_hit: 0.5}\n", ""), "unknown key answers.prices.anthropic/claude-opus-5-5.cache_hit"},
		{"an unknown bound", answersBlock(anthropicByFile, opusPrice, "  bounds: {retries: 2}\n"), "unknown key answers.bounds.retries"},
		{"a zero count", answersBlock(anthropicByFile, opusPrice, "  bounds: {model_calls: 0}\n"), "answers.bounds.model_calls must be a positive integer"},
		{"a decimal count", answersBlock(anthropicByFile, opusPrice, "  bounds: {gh_calls: 2.5}\n"), "answers.bounds.gh_calls must be an integer"},
		{"a duration past the timer bound", answersBlock(anthropicByFile, opusPrice, "  bounds: {answer_seconds: 2147484}\n"), "answers.bounds.answer_seconds must be at most 2147483"},
		{"a zero daily limit", answersBlock(anthropicByFile, opusPrice, "  bounds: {daily_usd: 0}\n"), "answers.bounds.daily_usd must be a positive number"},
		{"an unknown conversation_store key", answersBlock(anthropicByFile, opusPrice, "  conversation_store: {password: hunter2}\n"), "unknown key answers.conversation_store.password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.block), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
			}
		})
	}
}

// Neither Load nor LoadForValidation (`legion start --check-config`'s) runs api_key_command or reads
// api_key_file: a provider's key is read for each model call, never at load.
func TestLoadingAnswersReadsNoProviderKey(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	block := answersBlock(
		"    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key_command: touch "+marker+"}\n"+
			"    openai: {base_url: https://gateway.internal.example/openai, api_key_file: absent-key}\n",
		opusPrice+gptPrice, "")
	for name, load := range map[string]func(string, func(string) string) (Config, error){"Load": Load, "LoadForValidation": LoadForValidation} {
		if _, err := load(writeConfigFile(t, minimalFile+block), noEnv); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%s ran api_key_command (stat %s: %v)", name, marker, err)
		}
	}
}

// A provider's api_key_file is read again on every call: a key rewritten between two calls is the
// second call's, trimmed. A missing or blank file is refused naming the key and the path, never
// what the file held.
func TestAnswerCredentialReadsTheFileAgainOnEveryCall(t *testing.T) {
	path := writeConfigFile(t, minimalFile+answersBlock(anthropicByFile, opusPrice, ""))
	cfg, err := LoadForValidation(path, noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	credential := cfg.Answers.Providers[AnswerAnthropic].Credential
	keyFile := filepath.Join(filepath.Dir(path), "keys", "anthropic")
	ctx := context.Background()

	if _, err := credential.Read(ctx); err == nil || !strings.Contains(err.Error(), "answers.providers.anthropic.api_key_file names "+keyFile) {
		t.Fatalf("Read with no file = %v, want a refusal naming the key and %s", err, keyFile)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ contents, want string }{
		{"sk-fixture-first\n", "sk-fixture-first"},
		{"  sk-fixture-second  \n", "sk-fixture-second"},
	} {
		if err := os.WriteFile(keyFile, []byte(tc.contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := credential.Read(ctx); err != nil || got != tc.want {
			t.Fatalf("Read = %q, %v; want %q, the file as it is now", got, err, tc.want)
		}
	}
	if err := os.WriteFile(keyFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := credential.Read(ctx); err == nil || err.Error() != "answers.providers.anthropic.api_key_file names "+keyFile+", which is empty" {
		t.Fatalf("Read of a blank file = %q, %v; want it refused as empty", got, err)
	}
}

// A provider's api_key_command runs again on every call, its output trimmed. A command that prints
// nothing is refused, as is one that fails, naming its exit and stderr and never what it printed;
// and a call whose context ends ends the command.
func TestAnswerCredentialRunsTheCommandAgainOnEveryCall(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	credential := AnswerCredential{
		Command: `n=$(cat ` + count + ` 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > ` + count + `; printf ' sk-fixture-%s \n' "$n"`,
		key:     "answers.providers.openai",
	}
	ctx := context.Background()
	for _, want := range []string{"sk-fixture-1", "sk-fixture-2"} {
		if got, err := credential.Read(ctx); err != nil || got != want {
			t.Fatalf("Read = %q, %v; want %q, the command run again", got, err, want)
		}
	}

	for _, tc := range []struct{ name, command, want string }{
		{"no output", "printf '  \\n'", "answers.providers.openai.api_key_command produced empty output"},
		{"a failure", "echo sk-fixture-leaked; echo token endpoint refused >&2; exit 3", "answers.providers.openai.api_key_command failed (exit 3): token endpoint refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AnswerCredential{Command: tc.command, key: "answers.providers.openai"}.Read(ctx)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Read = %q, %v; want the refusal %q", got, err, tc.want)
			}
		})
	}

	ended, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := (AnswerCredential{Command: "sleep 30", key: "answers.providers.openai"}).Read(ended); err == nil || !strings.Contains(err.Error(), "answers.providers.openai.api_key_command did not finish") {
		t.Fatalf("Read past its context = %v, want it ended", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Read past its context took %s, want the command killed", elapsed)
	}
}
