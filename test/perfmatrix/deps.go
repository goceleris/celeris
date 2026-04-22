// Blank-imports to pin every competitor framework + driver dep in
// go.mod/go.sum. Wave-2 replaces each entry here with a real import
// inside the corresponding servers/<framework>/ package. Keeping the
// imports centralized during the scaffold phase means one place to bump
// versions and one place for `go mod tidy` to reach.

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
