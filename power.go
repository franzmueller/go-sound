// Copyright (c) 2026 InfAI (CC SES)

package main

import (
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var (
	pres  presence
	pubMu sync.Mutex // orders the enqueue of birth and playing publishes; never held while waiting for an ack

	mqttClientPtr atomic.Pointer[mqtt.Client]
)

const powerPublishTimeout = 2 * time.Second

// getClient returns the MQTT client, or nil before setupMqtt has created it.
func getClient() mqtt.Client {
	if p := mqttClientPtr.Load(); p != nil {
		return *p
	}
	return nil
}

func setClient(c mqtt.Client) { mqttClientPtr.Store(&c) }

// awaitToken waits for the token until the deadline and reports success.
func awaitToken(t mqtt.Token, deadline time.Time) bool {
	if t == nil {
		return false
	}
	d := time.Until(deadline)
	if d <= 0 {
		d = time.Millisecond
	}
	if !t.WaitTimeout(d) {
		return false
	}
	return t.Error() == nil
}

// enqueueBirth publishes the retained birth message with the state read at
// enqueue time. pubMu is held only around the Publish call, which fixes the
// order of messages on the wire.
func enqueueBirth(client mqtt.Client) (mqtt.Token, string) {
	if client == nil {
		return nil, ""
	}
	pubMu.Lock()
	defer pubMu.Unlock()
	state := pres.state()
	payload, err := birthPayload(localId, cfg.DeviceName, cfg.DeviceType, state)
	if err != nil {
		log.Println("birth message: marshal failed:", err)
		return nil, state
	}
	return client.Publish("device-manager/device/"+cfg.MQTT.ClientID, 1, true, payload), state
}

// enqueuePlaying publishes the playing value under pubMu, so it cannot
// overtake a suspend's playing=false. While suspended the value is forced to
// false; a sound event (fromSound) is not published at all then, and the
// resume republishes the current value. Returns nil when nothing was sent.
func enqueuePlaying(client mqtt.Client, value, fromSound bool) (mqtt.Token, bool) {
	if client == nil {
		return nil, value
	}
	pubMu.Lock()
	defer pubMu.Unlock()
	if pres.suppressed() {
		if fromSound {
			return nil, value
		}
		value = false
	}
	return client.Publish("event/"+localId+"/playing", 1, false, strconv.FormatBool(value)), value
}

func logBirth(ok bool, state string) {
	if ok {
		log.Println("Published birth message, state:", state)
	} else {
		log.Println("Failed to publish birth message, state:", state)
	}
}

func logPlaying(ok bool, value bool) {
	if ok {
		log.Println("Published playing:", value)
	} else {
		log.Println("Failed to publish playing:", value)
	}
}

func publishBirth(client mqtt.Client, deadline time.Time) bool {
	t, state := enqueueBirth(client)
	ok := awaitToken(t, deadline)
	logBirth(ok, state)
	return ok
}

func publishPlaying(client mqtt.Client, value bool, deadline time.Time) bool {
	t, sent := enqueuePlaying(client, value, false)
	ok := awaitToken(t, deadline)
	logPlaying(ok, sent)
	return ok
}

// ---- power / session transitions (called from the window thread) ----

func onSystemSuspend() {
	log.Println("Power: suspend")
	if !pres.setSuspended(true) {
		return
	}
	deadline := time.Now().Add(powerPublishTimeout)
	client := getClient()
	// Enqueue both back to back, then wait for both within one deadline.
	tPlay, sent := enqueuePlaying(client, false, false) // switches the subwoofer off
	tBirth, state := enqueueBirth(client)
	logPlaying(awaitToken(tPlay, deadline), sent)
	logBirth(awaitToken(tBirth, deadline), state)
}

func onSystemResume() {
	log.Println("Power: resume")
	pres.setSuspended(false)
	// Always republish: the connection may have been dropped or the broker
	// may have fired the last will while this process was frozen. Sound
	// events suppressed during suspend are covered by the current value.
	deadline := time.Now().Add(powerPublishTimeout)
	client := getClient()
	tBirth, state := enqueueBirth(client)
	tPlay, sent := enqueuePlaying(client, active.Load(), false)
	logBirth(awaitToken(tBirth, deadline), state)
	logPlaying(awaitToken(tPlay, deadline), sent)
}

func onSessionLock() {
	log.Println("Session: locked")
	if !pres.setLocked(true) {
		return
	}
	// playing is untouched; the monitor skips the silence timeout while locked.
	publishBirth(getClient(), time.Now().Add(powerPublishTimeout))
}

func onSessionUnlock() {
	log.Println("Session: unlocked")
	if !pres.setLocked(false) {
		return
	}
	// Same as resume: birth, then the current value (false if still suspended).
	deadline := time.Now().Add(powerPublishTimeout)
	client := getClient()
	tBirth, state := enqueueBirth(client)
	tPlay, sent := enqueuePlaying(client, active.Load(), false)
	logBirth(awaitToken(tBirth, deadline), state)
	logPlaying(awaitToken(tPlay, deadline), sent)
}
