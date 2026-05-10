module github.com/goceleris/celeris/test/drivercmp/redis

go 1.26.3

require (
	github.com/goceleris/celeris v0.0.0
	github.com/redis/go-redis/v9 v9.19.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/goceleris/celeris => ../../..
