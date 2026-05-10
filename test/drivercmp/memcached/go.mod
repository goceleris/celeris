module github.com/goceleris/celeris/test/drivercmp/memcached

go 1.26.3

require (
	github.com/bradfitz/gomemcache v0.0.0-20250403215159-8d39553ac7cf
	github.com/goceleris/celeris v0.0.0
)

require golang.org/x/sys v0.44.0 // indirect

replace github.com/goceleris/celeris => ../../..
