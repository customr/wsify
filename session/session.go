package session

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/customr/wsify/broker"
	"github.com/customr/wsify/config"
	"github.com/customr/wsify/utils"
	"golang.org/x/net/websocket"
)

type Session struct {
	Context      context.Context
	cancel       context.CancelFunc
	Broker       broker.Driver
	Config       *config.Config
	Conn         *websocket.Conn
	Message      Message
	DoneChannels map[string]chan struct{}
	mu           sync.RWMutex
	ErrChan      chan error
	Writer       chan []byte
	wg           sync.WaitGroup
	closed       bool
	closeMu      sync.RWMutex
	lastActivity time.Time
	pingTicker   *time.Ticker
}

func NewSession(ctx context.Context, broker broker.Driver, cfg *config.Config, conn *websocket.Conn) *Session {
	ctx, cancel := context.WithCancel(ctx)
	return &Session{
		Context:      ctx,
		cancel:       cancel,
		Broker:       broker,
		Config:       cfg,
		Conn:         conn,
		DoneChannels: make(map[string]chan struct{}),
		ErrChan:      make(chan error, 100),
		Writer:       make(chan []byte, 100),
		lastActivity: time.Now(),
	}
}

func (s *Session) Serve() error {
	// Set initial deadlines
	s.Conn.SetDeadline(time.Now().Add(60 * time.Second))

	// Start heartbeat
	s.pingTicker = time.NewTicker(30 * time.Second)
	s.wg.Add(1)
	go s.heartbeatLoop()

	// Start writer goroutine
	s.wg.Add(1)
	go s.writerLoop()

	// Start error handler
	s.wg.Add(1)
	go s.errorHandlerLoop()

	// Main message processing loop
	err := s.messageLoop()

	// Cleanup
	s.cleanup()

	return err
}

func (s *Session) heartbeatLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.pingTicker.C:
			// Check if connection is stale
			s.mu.Lock()
			idle := time.Since(s.lastActivity) > 60*time.Second
			s.mu.Unlock()

			if idle {
				s.Config.GetLogger().Debug("connection idle, sending ping")
				// Send a ping message (or any small message)
				s.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := websocket.Message.Send(s.Conn, `{"type":"ping"}`); err != nil {
					// Ping failed, connection is probably dead
					s.cancel()
					return
				}
			}
		case <-s.Context.Done():
			return
		}
	}
}

func (s *Session) writerLoop() {
	defer s.wg.Done()

	for {
		select {
		case output, ok := <-s.Writer:
			if !ok {
				return
			}

			// Set write deadline for this message
			if err := s.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				s.safeSendError(err)
				return
			}

			if err := websocket.Message.Send(s.Conn, string(output)); err != nil {
				// Only log non-EOF errors
				if !s.isNormalCloseError(err) {
					s.Config.GetLogger().Error("write error", "error", err)
				}
				// Trigger cleanup on write error
				s.cancel()
				return
			}

		case <-s.Context.Done():
			return
		}
	}
}

func (s *Session) errorHandlerLoop() {
	defer s.wg.Done()

	for {
		select {
		case err, ok := <-s.ErrChan:
			if !ok {
				return
			}
			// Log only important errors
			if err != nil && !s.isNormalCloseError(err) && !errors.Is(err, io.EOF) {
				s.Config.GetLogger().Error("session error", "error", err)
			}
		case <-s.Context.Done():
			return
		}
	}
}

func (s *Session) messageLoop() error {
	for {
		// Reset read deadline before each read
		if err := s.Conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			return err
		}

		var msg Message
		err := websocket.JSON.Receive(s.Conn, &msg)

		// Update last activity on any receive attempt
		s.mu.Lock()
		s.lastActivity = time.Now()
		s.mu.Unlock()

		if err != nil {
			// Handle different types of errors
			if s.isNormalCloseError(err) {
				return nil // Normal closure, exit cleanly
			}

			if errors.Is(err, io.EOF) {
				return nil // Client disconnected
			}

			// Check for timeout errors
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Timeout is expected, just continue and wait for next message
				continue
			}

			// For other errors, send to error channel but continue
			s.safeSendError(err)
			continue
		}

		// Process valid message
		s.Message = msg

		// Authorize message
		canProceed, err := utils.ShouldAcceptPayload(s.Config.GetAuthorizerEndpointURL(), s.Message)
		if err != nil {
			s.safeSendError(err)
			continue
		}

		if !canProceed {
			continue
		}

		// Handle command
		switch s.Message.Command {
		case MessageCommandTypeJoin:
			s.onJoin()
		case MessageCommandTypeLeave:
			s.onLeave()
		}
	}
}

func (s *Session) isNormalCloseError(err error) bool {
	if err == nil {
		return false
	}

	// Common normal closure errors
	errStr := err.Error()
	return errors.Is(err, io.EOF) ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "connection closed") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer")
}

func (s *Session) safeSendError(err error) {
	select {
	case s.ErrChan <- err:
	default:
		// Channel full, log directly
		s.Config.GetLogger().Debug("error channel full", "error", err)
	}
}

func (s *Session) cleanup() {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	s.closeMu.Unlock()

	// Stop ping ticker
	if s.pingTicker != nil {
		s.pingTicker.Stop()
	}

	// Cancel context to stop all goroutines
	s.cancel()

	// Close all subscription channels
	s.mu.Lock()
	for channel, doneCh := range s.DoneChannels {
		select {
		case doneCh <- struct{}{}:
		default:
		}
		close(doneCh)
		delete(s.DoneChannels, channel)
	}
	s.mu.Unlock()

	// Close writer channel
	close(s.Writer)

	// Wait for all goroutines with timeout
	waitChan := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waitChan)
	}()

	select {
	case <-waitChan:
		// All goroutines finished
	case <-time.After(5 * time.Second):
		s.Config.GetLogger().Warn("timeout waiting for goroutines to finish")
	}

	// Close error channel last
	close(s.ErrChan)

	// Close connection
	s.Conn.Close()

	s.Config.GetLogger().Info("session cleaned up", "channels", len(s.DoneChannels))
}

func (s *Session) onJoin() {
	channel := s.Message.GetArgsChannel()

	if channel == "" {
		s.safeSendError(errors.New("empty channel name"))
		return
	}

	// Check if already subscribed
	s.mu.Lock()
	if done, exists := s.DoneChannels[channel]; exists {
		// Verify subscription is still active by non-blocking check
		select {
		case done <- struct{}{}:
			// Old subscription still active, close it first
			close(done)
			delete(s.DoneChannels, channel)
		default:
			// Channel is blocked or closed, create new one
			delete(s.DoneChannels, channel)
		}
	}
	s.mu.Unlock()

	// Subscribe to broker
	feed, done, err := s.Broker.Subscribe(s.Context, channel)
	if err != nil {
		s.safeSendError(err)
		return
	}

	s.mu.Lock()
	s.DoneChannels[channel] = done
	s.mu.Unlock()

	// Start subscription handler
	s.wg.Add(1)
	go s.subscriptionHandler(channel, feed, done)
}

func (s *Session) subscriptionHandler(channel string, feed <-chan []byte, done chan struct{}) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.DoneChannels, channel)
		s.mu.Unlock()
	}()

	for {
		select {
		case msg, ok := <-feed:
			if !ok {
				// Feed closed, subscription ended
				return
			}

			// Try to send with timeout, but check context first
			select {
			case s.Writer <- msg:
				// Success
			case <-s.Context.Done():
				return
			case <-time.After(5 * time.Second):
				// Writer channel full, check if connection is still alive
				if !s.isConnectionAlive() {
					return
				}
				s.Config.GetLogger().Warn("writer channel full, retrying", "channel", channel)
				// Retry once
				select {
				case s.Writer <- msg:
				case <-s.Context.Done():
					return
				case <-time.After(2 * time.Second):
					s.Config.GetLogger().Warn("writer channel still full, dropping message", "channel", channel)
				}
			}

		case <-done:
			return
		case <-s.Context.Done():
			return
		}
	}
}

func (s *Session) isConnectionAlive() bool {
	// Try a non-blocking write to check connection
	s.Conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	defer s.Conn.SetWriteDeadline(time.Time{})

	err := websocket.Message.Send(s.Conn, `{"type":"ping"}`)
	return err == nil
}

func (s *Session) onLeave() {
	channel := s.Message.GetArgsChannel()

	s.mu.Lock()
	defer s.mu.Unlock()

	if done, exists := s.DoneChannels[channel]; exists {
		select {
		case done <- struct{}{}:
		default:
		}
		close(done)
		delete(s.DoneChannels, channel)
	}
}
