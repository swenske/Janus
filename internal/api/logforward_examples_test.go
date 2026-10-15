package api

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The log forwarding configurations the docs show (examples/logforward)
// are what janusctl system log-forwarding set sends: parsed and
// validated as the node does.
func TestLogForwardingExamples(t *testing.T) {
	files, err := filepath.Glob("../../examples/logforward/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples/logforward/*.json: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var p janusv1alpha1.LogForwardingConfig
		if err := protojson.Unmarshal(data, &p); err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		cfg := logConfigFromProto(&p)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if len(cfg.Targets) == 0 {
			t.Errorf("%s: no target", f)
		}
	}
}
