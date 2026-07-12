package runtime

import "testing"

func TestProviderCapabilities_ZeroValue(t *testing.T) {
	var caps ProviderCapabilities
	if caps.CanReportAttachment {
		t.Error("zero-value CanReportAttachment should be false")
	}
	if caps.CanReportActivity {
		t.Error("zero-value CanReportActivity should be false")
	}
}
