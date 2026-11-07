package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// gateRow is one stored design gate of LEGION-208, at version with the approval and the changes
// request a test gives it.
func gateRow(step record.DesignStep, artifact string, version int) record.DesignGate {
	return record.DesignGate{Issue: "LEGION-208", Step: step, ArtifactID: artifact, LatestVersion: version}
}

// Each step a deployment gates answers its own state, read from that step's own row: the four
// states a record can say, in the order the deployment gates them. A row of a step the deployment
// does not gate is in no entry, and a gated step with no row is answered all the same.
func TestGatedStepsAnswerEachConfiguredStepsOwnState(t *testing.T) {
	approvedAtFour, approvedBelowLatest := 4, 4
	criteriaApproved := gateRow(record.DesignStepCriteria, "4c1b8e2a-6f9d-4304-8b5e-1c8a2f6d940b", 4)
	criteriaApproved.ApprovedVersion = &approvedAtFour
	criteriaBehind := gateRow(record.DesignStepCriteria, "4c1b8e2a-6f9d-4304-8b5e-1c8a2f6d940b", 5)
	criteriaBehind.ApprovedVersion = &approvedBelowLatest
	toldAtTwo := 2
	specChanges := gateRow(record.DesignStepSpec, "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", 2)
	specChanges.ToldChangesVersion, specChanges.ToldChangesReason = &toldAtTwo, "name the rollback path"
	// A changes request the next version answered: the record keeps it at the version it was told
	// at, which is below the version Dispatch now holds, so the step waits on a person again.
	specAnswered := specChanges
	specAnswered.LatestVersion = 3
	approvedAtOne := 1
	decisionsApproved := gateRow(record.DesignStepDecisions, "9a8fd348-55d5-4d72-a793-3cd037f75f13", 1)
	decisionsApproved.ApprovedVersion = &approvedAtOne

	for _, tc := range []struct {
		name  string
		steps []record.DesignStep
		gates map[record.DesignStep]record.DesignGate
		want  []GatedStepView
	}{
		{
			name:  "an approved step, a step whose reviewer asked for changes, and a row of a step the deployment does not gate",
			steps: []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec},
			gates: map[record.DesignStep]record.DesignGate{
				record.DesignStepDecisions: decisionsApproved,
				record.DesignStepCriteria:  criteriaApproved,
				record.DesignStepSpec:      specChanges,
			},
			want: []GatedStepView{
				{Step: record.DesignStepCriteria, ArtifactID: criteriaApproved.ArtifactID, CurrentVersion: 4, ApprovedVersion: &approvedAtFour, Status: GatedStepApproved},
				{Step: record.DesignStepSpec, ArtifactID: specChanges.ArtifactID, CurrentVersion: 2, Status: GatedStepChangesRequested, ChangesReason: "name the rollback path"},
			},
		},
		{
			name:  "a step whose approval is a version behind, a step whose changes request a newer version answered, and a step nobody registered",
			steps: []record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria, record.DesignStepSpec},
			gates: map[record.DesignStep]record.DesignGate{
				record.DesignStepCriteria: criteriaBehind,
				record.DesignStepSpec:     specAnswered,
			},
			want: []GatedStepView{
				{Step: record.DesignStepDecisions, Status: GatedStepUnregistered},
				{Step: record.DesignStepCriteria, ArtifactID: criteriaBehind.ArtifactID, CurrentVersion: 5, ApprovedVersion: &approvedBelowLatest, Status: GatedStepWaiting},
				{Step: record.DesignStepSpec, ArtifactID: specAnswered.ArtifactID, CurrentVersion: 3, Status: GatedStepWaiting},
			},
		},
		{
			name:  "a deployment that gates no step, whose issue holds a gate all the same",
			steps: config.DesignGateOff.Steps(),
			gates: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: specChanges},
			want:  []GatedStepView{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GatedStepViewsOf(tc.steps, tc.gates)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("gatedSteps = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A deployment on `root-issues` gates the one step spec, so its root answers one entry, and that
// entry and the `designGate` beside it say the same of the same row: the wire a reader of one
// gate reads is what it was.
func TestGatedStepsUnderRootIssuesAnswerTheSpecStepBesideTheDesignGate(t *testing.T) {
	approved := 6
	spec := gateRow(record.DesignStepSpec, "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", 7)
	spec.ApprovedVersion = &approved
	gates := map[record.DesignStep]record.DesignGate{record.DesignStepSpec: spec}
	gate := record.SpecGate(gates)
	if gate == nil {
		t.Fatal("the spec row is the gate every reader of one gate takes, and it is not there")
	}
	issue := Issue{
		Key: "LEGION-208",
		DesignGate: &GateView{
			ArtifactID:      gate.ArtifactID,
			CurrentVersion:  gate.LatestVersion,
			ApprovedVersion: gate.ApprovedVersion,
		},
		GatedSteps: GatedStepViewsOf(config.DesignGateRootIssues.Steps(), gates),
	}

	encoded, err := json.Marshal(issue)
	if err != nil {
		t.Fatalf("marshal the issue: %v", err)
	}
	var body struct {
		DesignGate *GateView       `json:"designGate"`
		GatedSteps []GatedStepView `json:"gatedSteps"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("read the issue back: %v", err)
	}
	wantSteps := []GatedStepView{{
		Step:            record.DesignStepSpec,
		ArtifactID:      "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
		CurrentVersion:  7,
		ApprovedVersion: &approved,
		Status:          GatedStepWaiting,
	}}
	if !reflect.DeepEqual(body.GatedSteps, wantSteps) {
		t.Fatalf("gatedSteps = %+v, want %+v: one entry for the one step `root-issues` gates", body.GatedSteps, wantSteps)
	}
	wantGate := &GateView{ArtifactID: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", CurrentVersion: 7, ApprovedVersion: &approved}
	if !reflect.DeepEqual(body.DesignGate, wantGate) {
		t.Fatalf("designGate = %+v, want %+v: the spec step's row, as it was before the list", body.DesignGate, wantGate)
	}
}

// An issue with no gated step — a child, an issue the workflow does not record, every issue of a
// deployment that gates nothing — writes no `gatedSteps` key at all, as it writes no `designGate`:
// the wire carries no empty list, and a reader before this contract is served none of the field.
func TestGatedStepsOfAnIssueWithNoneAreLeftOffTheWire(t *testing.T) {
	encoded, err := json.Marshal(Issue{
		Key:        "LEGION-209",
		Phase:      Unrecorded,
		Status:     string(Unrecorded),
		GatedSteps: GatedStepViewsOf(config.DesignGateOff.Steps(), nil),
	})
	if err != nil {
		t.Fatalf("marshal the issue: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("read the issue back: %v", err)
	}
	if _, carries := body["gatedSteps"]; carries {
		t.Fatalf("issue = %s, want no gatedSteps key", encoded)
	}
}

// gatedStepsContract is the contract whose entry in version.go added the field, and this daemon's
// own contract is never below it: a caller at this contract is served the gated steps.
func TestGatedStepsContractIsTheContractThatAddedTheField(t *testing.T) {
	const addedGatedSteps = 55
	if gatedStepsContract != addedGatedSteps {
		t.Fatalf("gatedStepsContract = %d, want %d, the contract whose entry in version.go added the issues' gated steps",
			gatedStepsContract, addedGatedSteps)
	}
	if gatedStepsContract > DaemonAPIVersion {
		t.Fatalf("gatedStepsContract = %d, above DaemonAPIVersion %d: no caller could be served the field",
			gatedStepsContract, DaemonAPIVersion)
	}
}
