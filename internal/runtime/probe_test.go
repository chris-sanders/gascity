package runtime

import "testing"

func TestProbeResult_Constants(t *testing.T) {
	// Verify the three probe states are distinct.
	if ProbeAlive == ProbeDead || ProbeAlive == ProbeUnknown || ProbeDead == ProbeUnknown {
		t.Fatal("ProbeResult constants must be distinct")
	}
}

func TestProviderCapabilities_ZeroValue(t *testing.T) {
	var caps ProviderCapabilities
	if caps.CanReportAttachment {
		t.Error("zero-value CanReportAttachment should be false")
	}
	if caps.CanReportActivity {
		t.Error("zero-value CanReportActivity should be false")
	}
}
