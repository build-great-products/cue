// Command cue is the Cue process entry point.
package main

import (
	"context"
	"os"

	"github.com/build-great-products/cue/internal/app"
)

// version is replaced at build time with: -ldflags "-X main.version=<version>".
var version = "devel"

func main() {
	application := app.App{
		Out:     os.Stdout,
		Err:     os.Stderr,
		Version: version,
	}
	os.Exit(application.Run(context.Background(), os.Args[1:]))
}
