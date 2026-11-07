package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The four documents of LEGION-208 the step binding sorts between: the issue's primary document,
// its Verification document, its decisions document under the role Dispatch now gives it, and a
// decisions document stored before that role existed, which only its slug names. notesDoc is a
// document of the issue that is none of them.
const (
	specDoc          = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093"
	verificationDoc  = "4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d"
	decisionsRoleDoc = "7c1e3a5b-9d2f-4b6a-8e0c-1d3f5a7b9c2e"
	decisionsSlugDoc = "0544d460-0931-4374-b20b-790408519edd"
	notesDoc         = "1f2e3d4c-5b6a-4798-9c0d-2e4f6a8b0c1e"
)

// gateStepDocuments is the four documents and the plain one as Dispatch answers them, each on
// LEGION-208. The fake calls a document with neither a role nor a slug the issue's primary
// document (statusRecorder.Approval).
func gateStepDocuments() map[string]dispatch.Approval {
	return map[string]dispatch.Approval{
		specDoc:          {State: "none", LatestVersion: 4},
		verificationDoc:  {State: "none", LatestVersion: 4, Role: dispatch.ArtifactRoleVerification, Slug: "verification-md"},
		decisionsRoleDoc: {State: "none", LatestVersion: 4, Role: dispatch.ArtifactRoleDecisions, Slug: dispatch.DecisionsSlug},
		decisionsSlugDoc: {State: "none", LatestVersion: 4, Slug: dispatch.DecisionsSlug},
		notesDoc:         {State: "none", LatestVersion: 4, Slug: "notes-md"},
	}
}

// register_gate records one step of the deployment's design gate, and each step records one
// document of the issue: the spec step its primary document, the criteria step its Verification
// document, the decisions step its decisions document, by the role Dispatch gives it or, for a
// document stored before that role existed, by its slug.
//
// The registrations a deployment takes and the ones it refuses are driven together, so a build
// that refuses every registration fails on the accepted arms and one that takes any document for
// any step fails on the refused ones.
func TestRegisterGateStepRefusals(t *testing.T) {
	all := []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria, record.DesignStepSpec}
	for _, tc := range []struct {
		name     string
		steps    []record.DesignStep
		step     string
		document string
		// want is the step the accepted registration records; code and status the refusal.
		want   record.DesignStep
		code   string
		status int
		// says is what the refusal's message must name.
		says []string
	}{
		{name: "the spec step takes the issue's primary document", steps: all, step: "spec", document: specDoc, want: record.DesignStepSpec},
		{name: "the criteria step takes the Verification document", steps: all, step: "criteria", document: verificationDoc, want: record.DesignStepCriteria},
		{name: "the decisions step takes the document Dispatch gave the role", steps: all, step: "decisions", document: decisionsRoleDoc, want: record.DesignStepDecisions},
		{name: "the decisions step takes a document stored before the role, by its slug", steps: all, step: "decisions", document: decisionsSlugDoc, want: record.DesignStepDecisions},
		{
			name:  "a deployment gating one step takes a call that names none",
			steps: []record.DesignStep{record.DesignStepCriteria}, document: verificationDoc, want: record.DesignStepCriteria,
		},
		{
			// A deployment that gates no step asks no person, so neither the step the call names
			// nor the document it names decides anything: the registration records the spec step
			// and the workflow opens it at once.
			name:  "a deployment gating no step records the spec step, whatever the call names",
			steps: []record.DesignStep{}, step: "decisions", document: notesDoc, want: record.DesignStepSpec,
		},
		{
			name:  "a deployment gating several steps needs the call to name one",
			steps: all, document: specDoc,
			code: "GATE_STEP_REQUIRED", status: http.StatusBadRequest, says: []string{"decisions, criteria, spec"},
		},
		{
			name:  "a step the deployment does not gate",
			steps: []record.DesignStep{record.DesignStepSpec}, step: "criteria", document: verificationDoc,
			code: "GATE_STEP_NOT_CONFIGURED", status: http.StatusConflict, says: []string{"criteria", "spec"},
		},
		{
			name:  "the spec step on the Verification document",
			steps: all, step: "spec", document: verificationDoc,
			code: "GATE_STEP_DOCUMENT_MISMATCH", status: http.StatusConflict, says: []string{"spec", "the issue's primary document"},
		},
		{
			name:  "the criteria step on the primary document",
			steps: all, step: "criteria", document: specDoc,
			code: "GATE_STEP_DOCUMENT_MISMATCH", status: http.StatusConflict, says: []string{"criteria", "the issue's Verification document"},
		},
		{
			name:  "the decisions step on a document that is neither",
			steps: all, step: "decisions", document: notesDoc,
			code: "GATE_STEP_DOCUMENT_MISMATCH", status: http.StatusConflict, says: []string{"decisions", "the issue's decisions document"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses := newArchitectHarnessGating(t, tc.steps, nil, nil)
			statuses.approvals = gateStepDocuments()
			statuses.documents = map[string]string{}
			for id := range statuses.approvals {
				statuses.documents[id] = "LEGION-208"
			}
			seedTree(t, h, "LEGION-208")
			architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)

			body := map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": tc.document, "version": 4}
			if tc.step != "" {
				body["step"] = tc.step
			}
			recorder := h.request(http.MethodPost, "/legion/v1/gates/register", body, nil)
			if tc.code != "" {
				refusal := recorder.Body.String()
				assertFailure(t, recorder, tc.status, tc.code)
				for _, says := range tc.says {
					if !strings.Contains(refusal, says) {
						t.Fatalf("the refusal %s does not say %q", refusal, says)
					}
				}
				if got := facts.recorded(); len(got) != 0 {
					t.Fatalf("facts = %#v, want none for a refused registration", got)
				}
				return
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("gate register = %d: %s", recorder.Code, recorder.Body)
			}
			got := facts.recorded()
			if len(got) != 1 {
				t.Fatalf("facts = %#v, want the one registration", got)
			}
			registered, ok := got[0].(intake.GateRegistered)
			if !ok {
				t.Fatalf("fact = %#v, want a gate registration", got[0])
			}
			if registered.Step != tc.want || registered.ArtifactID != tc.document {
				t.Fatalf("the registration records step %q of %s, want step %q of %s", registered.Step, registered.ArtifactID, tc.want, tc.document)
			}
		})
	}
}
