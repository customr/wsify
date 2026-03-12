package config

import (
	"sync"
	"time"
)

type ConnectionLimits struct {
	mu               sync.RWMutex
	maxConnections   int
	currentConnections int
	connectionTimestamps map[string]time.Time
}

func NewConnectionLimits(maxConnections int) *ConnectionLimits {
	return &ConnectionLimits{
		maxConnections:       maxConnections,
		currentConnections:   0,
		connectionTimestamps: make(map[string]time.Time),
	}
}

func (cl *ConnectionLimits) Acquire(connID string) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	
	if cl.currentConnections >= cl.maxConnections {
		return false
	}
	
	cl.currentConnections++
	cl.connectionTimestamps[connID] = time.Now()
	return true
}

func (cl *ConnectionLimits) Release(connID string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	
	if _, exists := cl.connectionTimestamps[connID]; exists {
		cl.currentConnections--
		delete(cl.connectionTimestamps, connID)
	}
}

func (cl *ConnectionLimits) CleanupStale(timeout time.Duration) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	
	now := time.Now()
	for connID, timestamp := range cl.connectionTimestamps {
		if now.Sub(timestamp) > timeout {
			delete(cl.connectionTimestamps, connID)
			cl.currentConnections--
		}
	}
}