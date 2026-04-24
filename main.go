package main

import (
	"errors"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if !errors.Is(err, errSilent) {
			output.Error("%s", err)
		}
		os.Exit(1)
	}
}
