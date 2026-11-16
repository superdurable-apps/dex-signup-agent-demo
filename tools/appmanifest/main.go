// Command appmanifest prints dex-app.yaml as the canonical JSON that the
// platform reads, for the template's scripts, which parse no YAML themselves:
//
//	go run -C tools/appmanifest . ../../dex-app.yaml
//
// It refuses what the platform refuses (see package strictyaml) with the line
// and column. The platform's commit gate also checks the manifest's fields.
package main

import (
	"fmt"
	"os"

	"github.com/superdurable/dex-template-basic-process/tools/appmanifest/strictyaml"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: appmanifest <dex-app.yaml>")
		os.Exit(2)
	}
	contents, err := os.ReadFile(os.Args[1])
	if err == nil {
		contents, err = strictyaml.CanonicalJSON(contents)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dex-app.yaml:", err)
		os.Exit(1)
	}
	fmt.Println(string(contents))
}
