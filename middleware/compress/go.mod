module github.com/goceleris/celeris/middleware/compress

go 1.27.2

require (
	github.com/andybalholm/brotli v1.2.6
	github.com/goceleris/celeris v1.5.11
	github.com/klauspost/compress v1.20.3
)

require (
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

// In the monorepo, build against the in-tree celeris core so submodule
// tests catch core breakage immediately. External consumers ignore this
// directive — they pull whatever the `require` block above names.
replace github.com/goceleris/celeris => ../../
