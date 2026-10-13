module github.com/goceleris/celeris/test/drivercmp/redis

go 1.27.2

require (
	github.com/goceleris/celeris v1.5.11
	github.com/redis/go-redis/v9 v9.23.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/goceleris/celeris => ../../..
