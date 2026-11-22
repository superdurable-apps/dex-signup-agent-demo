package signup

import "testing"

// A turn may make as many model calls as under the pi harness, the Dex built-in loop's default of 100, not Eino's
// default MaxIterations of 20, at which the adapter ends the turn as failed.
func TestATurnMayMakeAsManyModelCallsAsUnderThePiHarness(t *testing.T) {
	if config := signupAgentConfig(nil, nil); config.MaxIterations != 100 {
		t.Fatalf("MaxIterations = %d", config.MaxIterations)
	}
}
