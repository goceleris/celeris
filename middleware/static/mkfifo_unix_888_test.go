//go:build !windows

package static

import "syscall"

func mkfifo888(path string) error { return syscall.Mkfifo(path, 0o644) }
