package celeris

import (
	"time"

	"github.com/goceleris/celeris/internal/ctxkit"
)

func init() {
	// middleware/static serves a file with validators taken from the open
	// descriptor (celeris#846); see ctxkit.FileFromDir.
	ctxkit.FileFromDir = func(c any, baseDir, userPath, contentType string,
		onOpen func(modTime time.Time, size int64) (bool, error)) error {
		return c.(*Context).fileFromDir(baseDir, userPath, contentType, onOpen)
	}
}
