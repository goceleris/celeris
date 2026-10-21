module github.com/goceleris/celeris/test/drivercmp/memcached

go 1.27.3

require (
	github.com/bradfitz/gomemcache v0.0.0-20260422231931-4d751bb6e37c
	github.com/goceleris/celeris v1.5.11
)

require golang.org/x/sys v0.49.0 // indirect

replace github.com/goceleris/celeris => ../../..
