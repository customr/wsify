package main

import (
	"github.com/customr/wsify/broker"
	_ "github.com/customr/wsify/broker/drivers/memory"
	_ "github.com/customr/wsify/broker/drivers/redis"
	"github.com/customr/wsify/config"
	"github.com/customr/wsify/routes"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"log"
	"runtime"
	"time"
	"sync"
	"sync/atomic"
)

var (
    activeConnections int64
    totalConnections  int64
    connMu            sync.Mutex
)

func main() {
	cfg, err := config.NewFromFlags()
	if err != nil {
		panic(err.Error())
	}

	brokerConn, err := broker.Connect(cfg.GetBrokerDriver(), cfg.GetBrokerDSN())
	if err != nil {
		panic(err.Error())
	}

	srv := echo.New()
	srv.HideBanner = true

	srv.Use(middleware.Recover())
	srv.Use(middleware.CORS())

	srv.GET("/ws", func(c echo.Context) error {
		atomic.AddInt64(&activeConnections, 1)
		atomic.AddInt64(&totalConnections, 1)
		
		defer atomic.AddInt64(&activeConnections, -1)
		
		return routes.WebsocketRouteHandler(cfg, brokerConn)(c)
	})
	srv.POST("/broadcast", routes.BroadcastHandler(cfg, brokerConn))

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		
		var m runtime.MemStats
		for range ticker.C {
			runtime.ReadMemStats(&m)
			
			cfg.GetLogger().Info("server stats",
				"alloc_mb", m.Alloc/1024/1024,
				"sys_mb", m.Sys/1024/1024,
				"goroutines", runtime.NumGoroutine(),
				"active_connections", atomic.LoadInt64(&activeConnections),
				"total_connections", atomic.LoadInt64(&totalConnections),
				"gc_cycles", m.NumGC,
			)
		}
	}()

	log.Fatal(srv.Start(cfg.GetWebServerListenAddr()))
}
