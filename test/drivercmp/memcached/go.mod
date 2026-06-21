module github.com/goceleris/celeris/test/drivercmp/memcached

go 1.26.4

require (
	github.com/bradfitz/gomemcache v0.0.0-20260422231931-4d751bb6e37c
	github.com/goceleris/celeris v0.0.0
)

require golang.org/x/sys v0.46.0 // indirect

replace github.com/goceleris/celeris => ../../..
