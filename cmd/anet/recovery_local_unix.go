//go:build !windows

package main

import "os"

// mkdirDurable makes the missing directory of a writeFileDurable file, 0700.
func mkdirDurable(dir string) error { return os.MkdirAll(dir, 0o700) }

// protectDurableTemp: on unix the temporary file is created 0600 and then set to perm (Chmod).
func protectDurableTemp(string, os.FileMode) error { return nil }

// renameDurable puts the temporary file in place.
func renameDurable(tmp, path string) error { return os.Rename(tmp, path) }
