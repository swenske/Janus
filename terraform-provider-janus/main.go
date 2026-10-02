// Command terraform-provider-janus is the Terraform (and OpenTofu)
// provider for Janus: it tells a Janus Controller to create, change and
// destroy Janus nodes on its hypervisors (docs/terraform.md).
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/swenske/Janus/terraform-provider-janus/internal/provider"
)

// version is set at build time (-X main.version).
var version = "dev"

func main() {
	debug := flag.Bool("debug", false, "run with support for debuggers like delve")
	flag.Parse()
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/swenske/janus",
		Debug:   *debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
