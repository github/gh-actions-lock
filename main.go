package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if !errors.Is(err, errSilent) {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}
		os.Exit(1)
	}
}
