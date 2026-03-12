package session

import (
	"context"
	"errors"
	"io"
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
		Writer:       make(chan []byte, 1000), // Increased buffer for better throughput
	}
}

func (s *Session) Serve() error {
	// DISABLE ALL DEADLINES - connections should stay open indefinitely
	// This is key for clients that only listen and don't send messages
	s.Conn.SetDeadline(time.Time{})
	s.Conn.SetReadDeadline(time.Time{})
	s.Conn.SetWriteDeadline(time.Time{})

	// Start heartbeat to keep connection alive through proxies
	s.pingTicker = time.NewTicker(30 * time.Second)
	s.wg.Add(1)
	go s.heartbeatLoop()

	// Start writer goroutine
	s.wg.Add(1)
	go s.writerLoop()

	// Start error handler
	s.wg.Add(1)
	go s.errorHandlerLoop()

	// Start message reader (optional - only if clients might send messages)
	// For pure server-push, you could even skip this entirely
	s.wg.Add(1)
	go s.messageReader()

	// Wait for context cancellation
	<-s.Context.Done()

	// Cleanup
	s.cleanup()

	return nil
}

// heartbeatLoop sends periodic pings to keep the connection alive through proxies
func (s *Session) heartbeatLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.pingTicker.C:
			// Send a ping message - this keeps the connection alive
			// and helps detect dead connections
			if err := s.safeWrite([]byte(`{"type":"ping"}`)); err != nil {
				// Ping failed, connection is probably dead
				s.cancel()
				return
			}
		case <-s.Context.Done():
			return
		}
	}
}

// safeWrite handles write operations with proper error handling
func (s *Session) safeWrite(data []byte) error {
	// Don't set any deadlines for writes
	if err := websocket.Message.Send(s.Conn, string(data)); err != nil {
		if !s.isNormalCloseError(err) {
			s.Config.GetLogger().Error("write error", "error", err)
		}
		return err
	}
	return nil
}

// writerLoop handles messages from the Writer channel
func (s *Session) writerLoop() {
	defer s.wg.Done()

	for {
		select {
		case output, ok := <-s.Writer:
			if !ok {
				return
			}

			if err := s.safeWrite(output); err != nil {
				// On write error, cancel the session
				s.cancel()
				return
			}

		case <-s.Context.Done():
			return
		}
	}
}

// messageReader handles incoming messages from clients
// Made optional - you can remove this if clients never send messages
func (s *Session) messageReader() {
	defer s.wg.Done()

	// Create a separate goroutine for reading without deadlines
	for {
		select {
		case <-s.Context.Done():
			return
		default:
			var msg Message
			// This will block until a message is received or connection breaks
			// No deadlines means it will block forever if no messages arrive
			err := websocket.JSON.Receive(s.Conn, &msg)

			if err != nil {
				// If there's an error reading, check if it's fatal
				if !s.isNormalCloseError(err) && !errors.Is(err, io.EOF) {
					s.Config.GetLogger().Error("read error", "error", err)
				}
				// On any read error, cancel the session
				// This handles cases where the client disconnects
				s.cancel()
				return
			}

			// Process the message in a separate goroutine to not block reading
			go s.processMessage(msg)
		}
	}
}

// processMessage handles individual messages from clients
func (s *Session) processMessage(msg Message) {
	// Authorize message
	canProceed, err := utils.ShouldAcceptPayload(s.Config.GetAuthorizerEndpointURL(), msg)
	if err != nil {
		s.safeSendError(err)
		return
	}

	if !canProceed {
		return
	}

	// Handle command
	switch msg.Command {
	case MessageCommandTypeJoin:
		s.onJoin(msg)
	case MessageCommandTypeLeave:
		s.onLeave(msg)
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

func (s *Session) onJoin(msg Message) {
	channel := msg.GetArgsChannel()

	if channel == "" {
		s.safeSendError(errors.New("empty channel name"))
		return
	}

	// Check if already subscribed
	s.mu.Lock()
	if done, exists := s.DoneChannels[channel]; exists {
		select {
		case done <- struct{}{}:
			close(done)
			delete(s.DoneChannels, channel)
		default:
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
				return
			}

			select {
			case s.Writer <- msg:
				// Success
			case <-s.Context.Done():
				return
			default:
				// Writer channel full, log and continue
				s.Config.GetLogger().Warn("writer channel full, message dropped", "channel", channel)
			}

		case <-done:
			return
		case <-s.Context.Done():
			return
		}
	}
}

func (s *Session) onLeave(msg Message) {
	channel := msg.GetArgsChannel()

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