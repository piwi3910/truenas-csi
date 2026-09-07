// Command truenas-csi runs the TrueNAS CSI driver as either the controller or the
// node plugin, selected by -mode.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/pwatteel/truenas-csi/internal/driver"
)

func main() {
	mode := flag.String("mode", "", "which plugin to run: controller or node")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", driver.DriverName, driver.Version)
		return
	}

	switch *mode {
	case "controller", "node":
		// Wired up in later tasks; the mode gate is the deliverable here.
		fmt.Printf("%s %s: %s mode not yet implemented\n", driver.DriverName, driver.Version, *mode)
	default:
		fmt.Fprintf(os.Stderr, "-mode must be %q or %q, got %q\n", "controller", "node", *mode)
		os.Exit(1)
	}
}
