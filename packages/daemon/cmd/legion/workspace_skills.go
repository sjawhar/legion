package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceSkillsUsage = "legion workspace-init skills --repo <owner>/<repo> --dir <dir>"

// workspaceSkills clones the deployment's skills marketplace (`runtime.kubernetes.skills_repository`)
// into the pod's skills volume and writes the three records beside it: the commit, the marketplace
// catalog's name and the plugins it names (workspace.CloneSkills). It is an issue pod's third init
// container, a second holder of the provisioning token in a container that mounts nothing an agent
// of the issue can write, so no role's launch ever clones anything: each role's start wrapper
// installs from the directory this filled, which is read-only everywhere else in the pod.
//
// It reads --repo before anything else, resolves git alone, and refuses before it touches the
// volume or its own TMPDIR, where the one-shot credential goes — every check (--repo, --dir, a
// readable provisioning token, git on PATH) runs first, so a refusal here leaves neither behind.
// The last line says which commit the pod's skills are at, which is the line a person reads to see
// what a pod installed.
func workspaceSkills(ctx context.Context, repo, dir string, stdout io.Writer) error {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("--dir must be an absolute path (got %q)", dir)
	}
	tokenFile, err := secretFileFromEnv(provisionTokenFileEnv)
	if err != nil {
		return err
	}
	token := func() (string, error) { return config.ReadSecretPointer(provisionTokenFileEnv, tokenFile) }
	tools, err := provisioningTools("git")
	if err != nil {
		return err
	}
	report, err := workspace.CloneSkills(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), workspace.SkillsRequest{
		Repo: repository, Token: token, CredentialDir: os.TempDir(), Dir: dir,
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init skills: "+line) },
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init skills: %s at %s (%s); marketplace %s with %s\n",
		repository, report.Commit, report.Branch, report.Marketplace, strings.Join(report.Plugins, ", "))
	return nil
}
