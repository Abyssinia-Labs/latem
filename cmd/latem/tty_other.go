//go:build !darwin && !(linux && (amd64 || arm64))

package main

import "os"

// isTTY has no implementation here. A release is built for macOS and Linux on
// amd64 and arm64, and everywhere else nothing is a terminal for `latem
// approve`'s purposes, so it refuses rather than approve without asking (A89).
func isTTY(*os.File) bool { return false }

// discardTypeAhead is never reached where isTTY is always false.
func discardTypeAhead(*os.File) error { return nil }
