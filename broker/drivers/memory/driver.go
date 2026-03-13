package memorybroker

import (
	"context"
	"sync"

	"github.com/savsgio/gotils/uuid"
)

type Driver struct {
	sync.RWMutex
	subscriptions map[string]map[string]chan []byte
	doneChannels  map[string]chan struct{}
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closed        chan struct{}
}

func (d *Driver) Connect(_ string) error {
	d.Lock()
	defer d.Unlock()

	d.subscriptions = make(map[string]map[string]chan []byte)
	d.doneChannels = make(map[string]chan struct{})
	d.closed = make(chan struct{})

	return nil
}

func (d *Driver) Subscribe(ctx context.Context, channel string) (<-chan []byte, chan struct{}, error) {
	d.Lock()

	id := uuid.V4()
	messagesChan := make(chan []byte, 100)
	doneChan := make(chan struct{}, 1)

	if _, found := d.subscriptions[channel]; !found {
		d.subscriptions[channel] = make(map[string]chan []byte)
	}

	d.subscriptions[channel][id] = messagesChan
	d.doneChannels[id] = doneChan
	d.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()

		select {
		case <-doneChan:
		case <-ctx.Done():
		case <-d.closed:
		}

		d.Lock()
		defer d.Unlock()

		// SAFELY remove subscription
		if ch, exists := d.subscriptions[channel][id]; exists {
			// Close the channel (receivers will see it's closed)
			close(ch)

			// Remove from subscriptions map
			delete(d.subscriptions[channel], id)

			// Clean up empty channel map
			if len(d.subscriptions[channel]) == 0 {
				delete(d.subscriptions, channel)
			}
		}
		delete(d.doneChannels, id)
	}()

	return messagesChan, doneChan, nil
}

func (d *Driver) Publish(_ context.Context, channel string, msg []byte) error {
	d.RLock()
	subscribers := make([]chan []byte, 0, len(d.subscriptions[channel]))
	for _, subscriber := range d.subscriptions[channel] {
		subscribers = append(subscribers, subscriber)
	}
	d.RUnlock()

	for _, subscriber := range subscribers {
		// SAFE SEND: Use select with recover to catch closed channel panics
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Channel was closed, just ignore
				}
			}()

			select {
			case subscriber <- msg:
				// Success
			default:
				// Channel full, skip to prevent blocking
			}
		}()
	}

	return nil
}

func (d *Driver) Close() error {
	d.closeOnce.Do(func() {
		close(d.closed)

		d.Lock()
		// Signal all done channels
		for id, ch := range d.doneChannels {
			select {
			case ch <- struct{}{}:
			default:
			}
			close(ch)
			delete(d.doneChannels, id)
		}
		d.Unlock()

		// Wait for all cleanup goroutines
		d.wg.Wait()

		d.Lock()
		// Now safe to close all subscription channels
		for channel, subs := range d.subscriptions {
			for id, ch := range subs {
				// Safe close with recover
				func() {
					defer func() { recover() }()
					close(ch)
				}()
				delete(subs, id)
			}
			delete(d.subscriptions, channel)
		}
		d.Unlock()
	})

	return nil
}
