package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	calls := os.Getenv("FAKE_GH_CALLS")
	f, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_, err = fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
	closeErr := f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, closeErr)
		os.Exit(2)
	}

	output := os.Getenv("FAKE_GH_OUTPUT")
	if output == "" {
		os.Exit(1)
	}
	fmt.Println(output)
}
