// Command deltav is the Delta-V control plane and its client. It is also
// installed as "dv"; behaviour is identical under either name.
package main

import (
	"os"

	"deltav/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args))
}
