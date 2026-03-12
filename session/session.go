package session

import (
	"context"
	"errors"
	"github.com/customr/wsify/broker"
	"github.com/customr/wsify/config"
	"github.com/customr/wsify/utils"
	"golang.org/x/net/websocket"
	"io"
	"sync"
	"time"
	"strings"
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
	// Set read/write deadlines
	s.Conn.SetDeadline(time.Now().Add(60 * time.Second))
	
	// Start heartbeat to detect dead connections
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
	
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			s.closeMu.RLock()
			if s.closed {
				s.closeMu.RUnlock()
				return
			}
			s.closeMu.RUnlock()
			
			// Check if connection is idle for too long
			s.mu.RLock()
			idle := time.Since(s.lastActivity) > 90*time.Second
			s.mu.RUnlock()
			
			if idle {
				s.Config.GetLogger().Info("closing idle connection", "idle_time", time.Since(s.lastActivity))
				s.cancel()
				return
			}
			
			// Send ping (if supported by websocket package)
			if err := websocket.Message.Send(s.Conn, "ping"); err != nil {
				return
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
			
			// Set write deadline
			s.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			
			if err := websocket.Message.Send(s.Conn, string(output)); err != nil {
				// Only log non-EOF errors
				if !s.isNormalCloseError(err) {
					s.Config.GetLogger().Error("write error", "error", err)
				}
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
		// Set read deadline
		s.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		
		var msg Message
		if err := websocket.JSON.Receive(s.Conn, &msg); err != nil {
			// Update last activity on error too to prevent premature cleanup
			s.mu.Lock()
			s.lastActivity = time.Now()
			s.mu.Unlock()
			
			// Check if it's a normal closure
			if s.isNormalCloseError(err) {
				return nil // Normal closure, no error
			}
			
			// Return only fatal errors
			if errors.Is(err, io.EOF) {
				return nil // Client disconnected normally
			}
			
			// Non-fatal error, continue
			s.safeSendError(err)
			continue
		}
		
		// Update last activity
		s.mu.Lock()
		s.lastActivity = time.Now()
		s.mu.Unlock()
		
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
	if _, exists := s.DoneChannels[channel]; exists {
		s.mu.Unlock()
		return
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
			
			// Try to send with timeout
			select {
			case s.Writer <- msg:
				// Success
			case <-time.After(5 * time.Second):
				s.Config.GetLogger().Warn("writer channel full, dropping message", "channel", channel)
			case <-s.Context.Done():
				return
			}
			
		case <-done:
			return
		case <-s.Context.Done():
			return
		}
	}
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
