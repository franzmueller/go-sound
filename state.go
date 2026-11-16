// Copyright (c) 2026 InfAI (CC SES)

package main

import (
	"encoding/json"
	"sync"
	"time"
)

// presence tracks the two conditions that make the PC "offline" for consumers:
// the session is locked, or the system is suspended / in standby.
type presence struct {
	mu        sync.Mutex
	locked    bool
	suspended bool
}

// birthState maps the lock/suspend state to the value of data.state.
func birthState(locked, suspended bool) string {
	if locked || suspended {
		return "offline"
	}
	return "online"
}

func (p *presence) state() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return birthState(p.locked, p.suspended)
}

// playingSuppressed reports whether playing must be reported as false and
// sound events must not be published. Only a suspended system does that; a
// locked session keeps following the sound.
func playingSuppressed(suspended bool) bool {
	return suspended
}

func (p *presence) suppressed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return playingSuppressed(p.suspended)
}

func (p *presence) isLocked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.locked
}

// silenceTimeout is the configured timeout, skipped (0) while the session is locked.
func silenceTimeout(base time.Duration, locked bool) time.Duration {
	if locked {
		return 0
	}
	return base
}

// setLocked stores the new value and reports whether it changed.
func (p *presence) setLocked(v bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := p.locked != v
	p.locked = v
	return changed
}

// setSuspended stores the new value and reports whether it changed.
func (p *presence) setSuspended(v bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := p.suspended != v
	p.suspended = v
	return changed
}

type birthData struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	DeviceType string `json:"device_type"`
}

type birthMessage struct {
	Method   string    `json:"method"`
	DeviceID string    `json:"device_id"`
	Data     birthData `json:"data"`
}

// birthPayload builds the retained device-manager message. encoding/json
// escapes names and types, so quotes in the device name cannot break it.
func birthPayload(deviceID, name, deviceType, state string) ([]byte, error) {
	return json.Marshal(birthMessage{
		Method:   "set",
		DeviceID: deviceID,
		Data:     birthData{Name: name, State: state, DeviceType: deviceType},
	})
}
