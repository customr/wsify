package session

import (
	"context"
	"errors"
	"github.com/alash3al/wsify/broker"
	"github.com/alash3al/wsify/config"
	"github.com/alash3al/wsify/utils"
	"golang.org/x/net/websocket"
	"io"
	"strings"
	"sync"
)

type Session struct {
	Context      context.Context
	Broker       broker.Driver
	Config       *config.Config
	Conn         *websocket.Conn
	Message      Message
	DoneChannels map[string]chan struct{}
	mu           sync.RWMutex // Protects DoneChannels
	ErrChan      chan error
	Writer       chan []byte
	cancel       context.CancelFunc
	wg           sync.WaitGroup // Tracks active goroutines
}

func (s *Session) Serve() error {
	ctx, cancel := context.WithCancel(s.Context)
	s.cancel = cancel
	defer cancel()

	s.wg.Add(1)
	go s.writerLoop(ctx)

	s.wg.Add(1)
	go s.errorHandlerLoop(ctx)

	defer s.cleanup()

	for {
		var msg Message
		if err := websocket.JSON.Receive(s.Conn, &msg); err != nil {
			if err == io.EOF || isWebSocketClosedError(err) {
				return err
			}

			s.sendError(err)
			continue
		}

		s.Message = msg

		canProceed, err := utils.ShouldAcceptPayload(s.Config.GetAuthorizerEndpointURL(), s.Message)
		if err != nil {
			return err
		}

		if !canProceed {
			continue
		}

		switch s.Message.Command {
		case MessageCommandTypeJoin:
			s.onJoin()
		case MessageCommandTypeLeave:
			s.onLeave()
		}
	}
}

func (s *Session) writerLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case output, ok := <-s.Writer:
			if !ok {
				return
			}
			if err := websocket.Message.Send(s.Conn, string(output)); err != nil {
				if !isWebSocketClosedError(err) && err != io.EOF {
					s.sendError(err)
				}
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Session) errorHandlerLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case err, ok := <-s.ErrChan:
			if !ok {
				return
			}
			if err != nil && !isWebSocketClosedError(err) && err != io.EOF {
				s.Config.GetLogger().Error(err.Error(), "func", "sessionErrorListener")
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Session) cleanup() {
	s.cancel()

	s.mu.Lock()
	for channel, done := range s.DoneChannels {
		select {
		case done <- struct{}{}:
		default:
		}
		close(done)
		delete(s.DoneChannels, channel)
	}
	s.mu.Unlock()

	if s.Writer != nil {
		close(s.Writer)
	}

	s.wg.Wait()

	if s.ErrChan != nil {
		close(s.ErrChan)
	}

	_ = s.Conn.Close()
}

func (s *Session) sendError(err error) {
	select {
	case s.ErrChan <- err:
	default:
		s.Config.GetLogger().Error("error channel full: "+err.Error(), "func", "sendError")
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

func (s *Session) onJoin() {
	channel := s.Message.GetArgsChannel()

	if channel == "" {
		s.sendError(errors.New("requested join on an empty chan"))
		return
	}

	s.mu.Lock()
	if existingDone, exists := s.DoneChannels[channel]; exists {
		select {
		case existingDone <- struct{}{}:
		default:
		}
		close(existingDone)
		delete(s.DoneChannels, channel)
	}
	s.mu.Unlock()

	feed, done, err := s.Broker.Subscribe(s.Context, channel)
	if err != nil {
		s.sendError(err)
		return
	}

	s.mu.Lock()
	s.DoneChannels[channel] = done
	s.mu.Unlock()

	s.wg.Add(1)
	go s.subscriptionHandler(channel, feed, done)
}

func (s *Session) subscriptionHandler(channel string, feed <-chan []byte, done chan struct{}) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		if ch, exists := s.DoneChannels[channel]; exists && ch == done {
			delete(s.DoneChannels, channel)
		}
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
			case <-s.Context.Done():
				return
			default:
				// Writer channel full, log and continue
				s.Config.GetLogger().Warn("writer channel full, dropping message", 
					"channel", channel)
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
