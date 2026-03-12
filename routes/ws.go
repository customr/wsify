package routes

import (
	"context"
	"github.com/customr/wsify/broker"
	"github.com/customr/wsify/config"
	"github.com/customr/wsify/session"
	"github.com/customr/wsify/utils"
	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"
	"net/http"
	"time"
	"strings"
)

func WebsocketRouteHandler(cfg *config.Config, brokerConn broker.Driver) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Authorize connection
		canConnect, err := utils.ShouldAcceptPayload(cfg.GetAuthorizerEndpointURL(), session.Message{
			Command: session.MessageCommandTypeConnect,
			Args: map[string]any{
				"headers": c.Request().Header,
				"query":   c.QueryParams(),
			},
		})
		
		if err != nil {
			cfg.GetLogger().Error("authorization error", "error", err)
			return c.NoContent(http.StatusForbidden)
		}
		
		if !canConnect {
			return c.NoContent(http.StatusForbidden)
		}
		
		// Upgrade to WebSocket
		websocket.Handler(func(conn *websocket.Conn) {
			// Create context with timeout
			ctx, cancel := context.WithTimeout(c.Request().Context(), 24*time.Hour)
			defer cancel()
			
			// Create session
			sess := session.NewSession(ctx, brokerConn, cfg, conn)
			
			// Serve session
			if err := sess.Serve(); err != nil {
				// Don't log normal closures as errors
				if !isNormalWebSocketClose(err) {
					cfg.GetLogger().Error("session serve error", "error", err)
				}
			}
		}).ServeHTTP(c.Response(), c.Request())
		
		return nil
	}
}

func isNormalWebSocketClose(err error) bool {
	if err == nil {
		return true
	}
	errStr := err.Error()
	return errStr == "EOF" ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "connection closed") ||
		strings.Contains(errStr, "broken pipe")
}