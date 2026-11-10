//go:build windows

package static

import "errors"

func mkfifo888(string) error { return errors.New("no FIFOs on Windows") }
