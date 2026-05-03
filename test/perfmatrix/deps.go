// Blank-imports under the perfmatrix_deps build tag to pin every
// competitor framework + driver in go.mod/go.sum so `go mod tidy`
// keeps them, even when no scenario is currently importing them.

//go:build perfmatrix_deps

package perfmatrix

import (
	_ "github.com/bradfitz/gomemcache/memcache"
	_ "github.com/cloudwego/hertz/pkg/app/server"
	_ "github.com/gin-gonic/gin"
	_ "github.com/go-chi/chi/v5"
	_ "github.com/gofiber/fiber/v3"
	_ "github.com/gorilla/sessions"
	_ "github.com/hertz-contrib/http2"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/kataras/iris/v12"
	_ "github.com/labstack/echo/v4"
	_ "github.com/magefile/mage/mg"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/valyala/fasthttp"
)
