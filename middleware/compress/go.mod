module github.com/goceleris/celeris/middleware/compress

go 1.26.0

require (
	github.com/andybalholm/brotli v1.2.1
	github.com/goceleris/celeris v1.4.1
	github.com/klauspost/compress v1.18.6
)

require (
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
)

// In the monorepo, build against the in-tree celeris core so submodule
// tests catch core breakage immediately. External consumers ignore this
// directive — they pull whatever the `require` block above names.
replace github.com/goceleris/celeris => ../../
