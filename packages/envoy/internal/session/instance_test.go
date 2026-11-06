package session

import "testing"

// A soft claim naming a predecessor session asks InstanceMatches whether the claimant and that
// predecessor belong to the same client instance. Two non-empty ids must be equal, and an empty
// id on either side is a client that predates the field, which keeps the old rule rather than
// stranding a session that cannot name its instance.
func TestInstanceMatches(t *testing.T) {
	const instanceA = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	const instanceB = "9e8b7d6c5a4f1c2a9e8b7d6c5a4f1c2a"

	cases := []struct {
		name      string
		stored    string
		requested string
		want      bool
	}{
		{
			name:      "same instance claims its own predecessor",
			stored:    instanceA,
			requested: instanceA,
			want:      true,
		},
		{
			name:      "another instance's live session is not the claimant's predecessor",
			stored:    instanceA,
			requested: instanceB,
			want:      false,
		},
		{
			name:      "holder registered by a client predating the field",
			stored:    "",
			requested: instanceA,
			want:      true,
		},
		{
			name:      "claimant from a client predating the field",
			stored:    instanceA,
			requested: "",
			want:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InstanceMatches(tc.stored, tc.requested); got != tc.want {
				t.Fatalf("InstanceMatches(%q, %q) = %v, want %v", tc.stored, tc.requested, got, tc.want)
			}
		})
	}
}
