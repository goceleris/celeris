module github.com/goceleris/celeris/test/benchcmp

go 1.26.0

require (
	github.com/go-chi/chi/v5 v5.2.5
	github.com/goceleris/celeris v1.3.3
	github.com/gofiber/fiber/v3 v3.2.0
	github.com/labstack/echo/v4 v4.15.1
	github.com/rs/cors v1.11.1
	github.com/valyala/fasthttp v1.70.0
)

require (
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/gofiber/schema v1.7.1 // indirect
	github.com/gofiber/utils/v2 v2.0.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/labstack/gommon v0.4.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.21 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	golang.org/x/crypto v0.50.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	golang.org/x/time v0.14.0 // indirect
)

replace (
	github.com/goceleris/celeris => ../../
	github.com/goceleris/celeris/middleware/protobuf => ../../middleware/protobuf
)
