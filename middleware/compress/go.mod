module github.com/goceleris/celeris/middleware/compress

go 1.26.4

require (
	github.com/andybalholm/brotli v1.2.2
	github.com/goceleris/celeris v1.5.5
	github.com/klauspost/compress v1.19.0
)

require (
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

// In the monorepo, build against the in-tree celeris core so submodule
// tests catch core breakage immediately. External consumers ignore this
// directive — they pull whatever the `require` block above names.
replace github.com/goceleris/celeris => ../../
