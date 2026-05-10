// Command gin serves the probatorium contract endpoints with the Gin
// framework. Two engine modes via -engine: h1 (plain HTTP/1.1) and h2c
// (h2c.NewHandler-wrapped Gin engine).
//
// Gin is set to ReleaseMode at startup so its debug logger is disabled
// — the benchmark cost of zap-level structured logging would dominate
// the small-response cells. UseRawPath is enabled so encoded path
// segments survive the router unchanged.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/goceleris/probatorium/servers/common"
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	engine := flag.String("engine", "h1", "runtime engine: h1 or h2c")
	flag.Parse()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.UseRawPath = true
	registerRoutes(r)

	var handler http.Handler = r
	if *engine == "h2c" {
		handler = h2c.NewHandler(r, &http2.Server{})
	}

	srv := &http.Server{Addr: *bind, Handler: handler}
	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("gin: listen %s: %v", *bind, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("gin: serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func registerRoutes(r *gin.Engine) {
	r.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "Hello, World!")
	})
	r.GET("/json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(`{"message":"Hello, World!"}`))
	})
	r.GET("/json-1k", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", common.JSON1KPayload())
	})
	r.GET("/json-64k", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", common.JSON64KPayload())
	})
	r.GET("/users/:id", func(c *gin.Context) {
		id := c.Param("id")
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "User ID: "+id)
	})
	r.POST("/upload", func(c *gin.Context) {
		_, _ = c.GetRawData()
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "OK")
	})
}
