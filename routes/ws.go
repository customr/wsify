package routes

import (
	"context"
	"github.com/alash3al/wsify/broker"
	"github.com/alash3al/wsify/config"
	"github.com/alash3al/wsify/session"
	"github.com/alash3al/wsify/utils"
	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"
	"io"
	"net/http"
	"strings"
	"time"
)

func WebsocketRouteHandler(cfg *config.Config, brokerConn broker.Driver) echo.HandlerFunc {
	return func(c echo.Context) error {
		canConnect, err := utils.ShouldAcceptPayload(cfg.GetAuthorizerEndpointURL(), session.Message{
			Command: session.MessageCommandTypeConnect,
			Args: map[string]any{
				"headers": c.Request().Header,
				"query":   c.QueryParams(),
			},
		})

		if err != nil {
			cfg.GetLogger().Error(err.Error(), "utils.ShouldAcceptPayload")
			return c.NoContent(http.StatusForbidden)
		}

		if !canConnect {
			return c.NoContent(http.StatusForbidden)
		}

		websocketHandler := websocket.Handler(func(conn *websocket.Conn) {
			ctx, cancel := context.WithTimeout(c.Request().Context(), 24*time.Hour)
			defer cancel()

			sess := &session.Session{
				Context:      ctx,
				Broker:       brokerConn,
				Config:       cfg,
				Conn:         conn,
				DoneChannels: make(map[string]chan struct{}),
				ErrChan:      make(chan error, 100),
				Writer:       make(chan []byte, 100),
			}

			if err := sess.Serve(); err != nil {
				if !isWebSocketClosedError(err) && err != io.EOF && err != context.Canceled {
					cfg.GetLogger().Error(err.Error(), "func", "session.Serve")
				}
			}
		})

		return echo.WrapHandler(websocket.Server{
			Handshake: func(cfg *websocket.Config, req *http.Request) error {
				return nil
			},
			Handler: websocketHandler,
		})(c)
	}
}

func isWebSocketClosedError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "closed") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer")
}