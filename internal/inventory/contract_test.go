package inventory

import (
	"os"
	"testing"
)

// TestSampleIsTheUploadContract pins the document this tool uploads. The
// komolos control plane is private and keeps its own copy of the limits, and
// tests its parser against an identical copy of testdata/sample-inventory.json.
// If this fails, update the sample and the control plane together.
func TestSampleIsTheUploadContract(t *testing.T) {
	b, err := os.ReadFile("testdata/sample-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := Decode(b)
	if err != nil {
		t.Fatalf("sample no longer decodes: %v", err)
	}
	if inv.SchemaVersion != SchemaVersion {
		t.Fatalf("sample schema %q, code schema %q", inv.SchemaVersion, SchemaVersion)
	}
	if MaxBytes != 10<<20 {
		t.Fatalf("MaxBytes changed to %d; the control plane rejects uploads over 10 MiB", MaxBytes)
	}
}
