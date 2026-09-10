package platform

import "testing"

func TestCloudMetrics_RegisterAndRecord(t *testing.T) {
	before := APIErrorCount(PlatformAWS, OpDiscover)
	RecordCloudAPIError(PlatformAWS, OpDiscover)
	if got := APIErrorCount(PlatformAWS, OpDiscover); got != before+1 {
		t.Fatalf("counter: got %v want %v", got, before+1)
	}
	SetCloudPeersManaged(PlatformAzure, 2)
	if got := PeersManagedValue(PlatformAzure); got != 2 {
		t.Fatalf("gauge: got %v want 2", got)
	}
}
