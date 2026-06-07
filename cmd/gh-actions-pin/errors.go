package main

import "errors"

// errSilent is returned by command run functions when blocking findings have
// already been reported through well-formed output (e.g. JSON on stdout). It
// maps to exit code 1 in Execute without printing a second error line.
var errSilent = errors.New("silent error")
