//go:build windows

package celeris

import "os"

func openNonblock(path string) (*os.File, error) { return os.Open(path) }
