// Command killport inspects and frees local TCP and UDP ports.
package main

import (
	"os"

	"github.com/paarths-collab/killport/internal/cli"
)

// Version is injected by GoReleaser and package managers with -X main.Version.
var Version = "dev"

func main() {
	os.Exit(cli.Main(Version))
}
