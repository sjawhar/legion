package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// conversationsDSN is the conversation store's URL a test's answers.conversation_store.dsn_file
// holds; its password opens nothing.
const conversationsDSN = "postgres://legion-daemon:fixture-conversations-password@conversations.internal.example:5432/omp_sessions"

// answersConfig is an `answers` block whose one provider's key command, were it ever run, would
// leave marker behind, and whose conversation store's URL is in dsnFile ("" for none).
func answersConfig(marker, dsnFile string) *config.Answers {
	return &config.Answers{
		Providers: map[config.AnswerProvider]config.AnswerEndpoint{
			config.AnswerOpenAI: {BaseURL: "https://gateway.internal.example/openai", Credential: config.AnswerCredential{Command: "touch " + marker}},
		},
		Prices:            map[string]config.AnswerPrice{"openai/gpt-6.1": {Input: 1.25, Output: 10}},
		ConversationStore: config.ConversationStore{DSNFile: dsnFile},
	}
}

// dsnFileOf writes contents to a conversation store URL file of its own and answers its path.
func dsnFileOf(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conversations-dsn")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A Kubernetes daemon runs no host gh of its own, so without `answers` it boots on a host with
// none; with `answers` it resolves the real gh the answer loop's gh tool runs at boot, as a tmux
// daemon resolves its own, and refuses to start, naming gh and its override, when there is none.
// The gh it resolves, LEGION_GH_PATH's here, and the conversation store's URL are boot's reads,
// and neither boot nor `legion start --check-config` runs the provider's key command.
func TestAKubernetesDaemonWithAnswersRefusesToStartWithoutGH(t *testing.T) {
	noGH := []string{"PATH=" + t.TempDir()}
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	if _, err := prepare(cfg, quietLogger(), overrides{environ: noGH}); err != nil {
		t.Fatalf("prepare with no answers and no gh = %v, want the boot to go on", err)
	}

	marker := filepath.Join(t.TempDir(), "ran")
	cfg.Answers = answersConfig(marker, dsnFileOf(t, conversationsDSN+"\n"))
	_, err := prepare(cfg, quietLogger(), overrides{environ: noGH})
	const want = "answers needs gh on the daemon's host, which the answer loop's gh tool runs, and none was found: set LEGION_GH_PATH to an absolute executable path, or put gh on PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("prepare with answers and no gh = %v, want %q", err, want)
	}

	gh := binary(t, t.TempDir(), "gh")
	withGH := environLookup(append(noGH, "LEGION_GH_PATH="+gh))
	reads, err := readBoot(cfg, withGH, os.Getenv, true, quietLogger())
	if err != nil {
		t.Fatalf("readBoot with answers and LEGION_GH_PATH: %v", err)
	}
	if reads.answers.gh != gh || reads.answers.conversationDSN != conversationsDSN {
		t.Errorf("boot's answer reads = gh %q, dsn %q; want %q and the file's URL", reads.answers.gh, reads.answers.conversationDSN, gh)
	}
	if _, _, err := CheckStart(cfg, withGH); err != nil {
		t.Fatalf("CheckStart with answers and LEGION_GH_PATH: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("boot or the check ran api_key_command (stat %s: %v)", marker, err)
	}
}

// A tmux daemon with `answers` and no repository, whose own tools boot does not resolve, still
// resolves gh for the loop, the first gh on PATH, and refuses to start without one; with a
// repository the loop's gh is the one boot resolved for the daemon's own tools.
func TestATmuxDaemonWithAnswersResolvesGHWithOrWithoutARepository(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cfg := testConfig(t)
	cfg.Answers = answersConfig(marker, "")
	empty := t.TempDir()
	if _, err := readBoot(cfg, environLookup([]string{"PATH=" + empty}), os.Getenv, false, quietLogger()); err == nil || !strings.HasPrefix(err.Error(), "answers needs gh on the daemon's host") {
		t.Fatalf("readBoot with answers and no gh = %v, want the refusal naming gh", err)
	}
	onPath := t.TempDir()
	gh := binary(t, onPath, "gh")
	reads, err := readBoot(cfg, environLookup([]string{"PATH=" + onPath}), os.Getenv, false, quietLogger())
	if err != nil {
		t.Fatalf("readBoot with answers and gh on PATH: %v", err)
	}
	if reads.answers.gh != gh || reads.tmux.tools != nil {
		t.Errorf("boot's answer gh = %q and tools %v, want %q and no daemon tools without a repository", reads.answers.gh, reads.tmux.tools, gh)
	}

	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	executable(t, onPath, "git")
	fakeJJ(t, onPath, currentJJ)
	if reads, err = readBoot(cfg, environLookup([]string{"PATH=" + onPath}), os.Getenv, false, quietLogger()); err != nil {
		t.Fatalf("readBoot with answers and a repository: %v", err)
	}
	if reads.answers.gh != reads.tmux.tools["gh"] || reads.answers.gh != gh {
		t.Errorf("boot's answer gh = %q, want the daemon tools' gh %q", reads.answers.gh, reads.tmux.tools["gh"])
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("boot ran api_key_command (stat %s: %v)", marker, err)
	}
}

// The conversation store's URL file is read at boot as every other *_file is: a missing or blank
// file, or one holding no postgres:// URL, refuses the boot naming the key and the file and never
// what the file holds.
func TestBootRefusesAConversationStoreFileWithNoURL(t *testing.T) {
	gh := binary(t, t.TempDir(), "gh")
	withGH := environLookup([]string{"PATH=" + t.TempDir(), "LEGION_GH_PATH=" + gh})
	missing := filepath.Join(t.TempDir(), "absent")
	notAURL := dsnFileOf(t, "host=conversations.internal.example password=fixture-keyword-password\n")
	blank := dsnFileOf(t, "\n")
	for _, tc := range []struct{ name, file, want string }{
		{"missing", missing, "answers.conversation_store.dsn_file names " + missing + ", which could not be read"},
		{"blank", blank, "answers.conversation_store.dsn_file names " + blank + ", which is empty"},
		{"no URL", notAURL, "answers.conversation_store.dsn_file names " + notAURL + ", which holds no postgres:// URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), tc.file)
			_, err := readBoot(cfg, withGH, os.Getenv, true, quietLogger())
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || strings.Contains(err.Error(), "fixture-keyword-password") {
				t.Fatalf("readBoot = %v, want a refusal starting %q that carries nothing the file holds", err, tc.want)
			}
		})
	}
}
