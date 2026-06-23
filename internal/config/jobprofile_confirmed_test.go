package config

import "testing"

func TestJobProfile_AwaitingConfirmation(t *testing.T) {
	falseVal := false
	trueVal := true

	cases := []struct {
		name string
		job  JobProfile
		want bool
	}{
		{"nil-confirmed-handwritten", JobProfile{Command: "/bin/echo"}, false},
		{"explicit-false-agent-defined", JobProfile{Command: "/bin/echo", Confirmed: &falseVal}, true},
		{"explicit-true-confirmed", JobProfile{Command: "/bin/echo", Confirmed: &trueVal}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.job.AwaitingConfirmation(); got != tc.want {
				t.Fatalf("AwaitingConfirmation() = %v, want %v", got, tc.want)
			}
		})
	}
}
