// Command overdub takes over the Echo Dot's action button and presents the Dot
// to Home Assistant as an ESPHome device, leaving Amazon's stack alone.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	var flags config
	flag.StringVar(&flags.Name, "name", "", "unique device name Home Assistant identifies this Dot by (required)")
	flag.Parse()

	if flags.Name == "" {
		fmt.Fprintln(os.Stderr, "overdub: -name is required, and must be unique on the network")
		os.Exit(2)
	}

	if err := checkName(flags.Name); err != nil {
		fmt.Fprintf(os.Stderr, "overdub: %v\n", err)
		os.Exit(2)
	}

	if err := serve(flags); err != nil {
		log.Printf("overdub: %v", err)
		os.Exit(1)
	}
}

type config struct {
	Name string
}

func checkName(name string) error {
	if len(name) > 63 {
		return fmt.Errorf("-name is %d characters; a DNS label stops at 63", len(name))
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("-name must not start or end with -; that is not a valid DNS label")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return fmt.Errorf("-name must be lowercase letters, digits, - or _: %q", name)
		}
	}
	return nil
}
