package intake

import "testing"

// Every Legion role appends a footer to a pull-request review it submits, naming the session that
// wrote it (skills/legion-worker/SKILL.md, prompts/roles/reviewer.md). LegionSession finds the
// last one, since a review may quote an earlier footer (a reply, a reviewer's own draft) before
// appending its own: workflow's byReviewer tells the reviewer's own review-App session from any
// other's by it.
func TestLegionSessionFindsTheLastFooterInAReviewsBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "a footer at the end, after a multi-line body",
			body: "Looks good.\n\nA few nits, but nothing blocking.\n\n<!-- legion: {\"session\":\"01a11940-9d2f-723c-b7de-d65a2e3ce427\",\"phase\":\"review\"} -->",
			want: "01a11940-9d2f-723c-b7de-d65a2e3ce427",
		},
		{name: "no footer", body: "Looks good.", want: ""},
		{
			name: "two footers; the last one wins",
			body: "<!-- legion: {\"session\":\"earlier\",\"phase\":\"review\"} -->\n\nActually, one more thing.\n\n<!-- legion: {\"session\":\"later\",\"phase\":\"review\"} -->",
			want: "later",
		},
		{name: "malformed JSON", body: "<!-- legion: {not json} -->", want: ""},
		{
			name: "a CRLF body",
			body: "Looks good.\r\n\r\n<!-- legion: {\"session\":\"crlf-session\",\"phase\":\"review\"} -->",
			want: "crlf-session",
		},
		{
			name: "the footer is not the body's last text; the last occurrence still wins",
			body: "<!-- legion: {\"session\":\"the-session\",\"phase\":\"review\"} -->\n\nedit: fixed a typo above.",
			want: "the-session",
		},
		{name: "an empty session field", body: "<!-- legion: {\"session\":\"\",\"phase\":\"review\"} -->", want: ""},
		{name: "the body is exactly the footer", body: "<!-- legion: {\"session\":\"only-thing-here\",\"phase\":\"review\"} -->", want: "only-thing-here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := PullRequestReview{Body: tc.body}
			if got := review.LegionSession(); got != tc.want {
				t.Fatalf("LegionSession() of %q = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
