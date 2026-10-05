package delivery

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func testSettings() model.DeliverySettings {
	return model.DeliverySettings{
		DeployRepo:        "acme/widgets",
		PopulationAuthors: []string{"octocat", "octocat-agent", "octocat-agent[bot]"},
		ExcludedRepos:     []string{"acme/dojo", "acme/widgets-smoke"},
	}
}

func testWindow() TimeWindow {
	return TimeWindow{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC),
	}
}

func TestIsPopulationPR(t *testing.T) {
	settings := testSettings()
	window := testWindow()
	inWindow := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		pr      RawPullRequest
		want    bool
		wantErr bool
	}{
		{
			name: "excluded repo wins even for a population author",
			pr: RawPullRequest{
				Repo: "acme/dojo", Author: "octocat", MergedAt: inWindow,
			},
			want: false,
		},
		{
			name: "author not in the list",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "someone-else", MergedAt: inWindow,
			},
			want: false,
		},
		{
			name: "before window start",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat",
				MergedAt: window.Start.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "at window end exactly is excluded (End is exclusive)",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: window.End,
			},
			want: false,
		},
		{
			name: "at window start exactly is included (Start is inclusive)",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: window.Start,
				Labels: []string{"non-task"},
			},
			want: true,
		},
		{
			name: "deploy-repo PR with task label is excluded",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: inWindow,
				Labels: []string{"task"},
			},
			want: false,
		},
		{
			name: "deploy-repo PR with non-task label is included",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: inWindow,
				Labels: []string{"non-task"},
			},
			want: true,
		},
		{
			name: "deploy-repo PR with neither label propagates the error",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: inWindow,
				Labels: nil,
			},
			wantErr: true,
		},
		{
			name: "non-deploy-repo PR by a population author is never excluded as a task PR",
			pr: RawPullRequest{
				Repo: "acme/other", Author: "octocat", MergedAt: inWindow,
				Labels: nil,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsPopulationPR(tt.pr, settings, window)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("IsPopulationPR() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsRework(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		{"fix #42", true},
		{"Fix #42", true},
		{"FIX #42", true},
		{"revert #42", true},
		{"hotfix #42", true},
		{"prefix something", false},
		{"feat: add widget", false},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := IsRework(tt.title); got != tt.want {
				t.Errorf("IsRework(%q) = %v, want %v", tt.title, got, tt.want)
			}
		})
	}
}

func TestRevertKind(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Revert #42", "revert"},
		{"Fix #42", "fix"},
		{"Hotfix #42", "fix"},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := RevertKind(tt.title); got != tt.want {
				t.Errorf("RevertKind(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
