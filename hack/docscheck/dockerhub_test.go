package docscheck

import (
	"os"
	"strings"
	"testing"
)

// Docker Hub's page (dashboard/dockerhub-overview.md, uploaded by
// image-build.yml) can't include a file: it shows the Compose setup
// itself, and must show the one the docs show and examples-check
// validates - its first yaml block is examples/compose/compose.yaml.
func TestDockerHubOverviewShowsTheComposeExample(t *testing.T) {
	overview, err := os.ReadFile("../../dashboard/dockerhub-overview.md")
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../examples/compose/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, block, found := strings.Cut(string(overview), "\n```yaml\n")
	if !found {
		t.Fatal("dashboard/dockerhub-overview.md has no yaml block: it should show examples/compose/compose.yaml")
	}
	block, _, found = strings.Cut(block, "\n```\n")
	if !found {
		t.Fatal("dashboard/dockerhub-overview.md: its yaml block never ends")
	}
	if block+"\n" != string(example) {
		t.Error("dashboard/dockerhub-overview.md's yaml block differs from examples/compose/compose.yaml: copy the file into it")
	}
}
