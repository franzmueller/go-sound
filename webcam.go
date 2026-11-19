// Copyright (c) 2026 InfAI (CC SES)

package main

import (
	"log"
	"sync/atomic"
	"time"
)

// webcamActive is the state of the camera as Windows reports it, before lock
// and suspend are applied.
var webcamActive atomic.Bool

// webcamChanged wakes the publisher. One pending signal is enough: the
// publisher always sends the current state.
var webcamChanged = make(chan struct{}, 1)

// setWebcam records a camera state and wakes the publisher without blocking
// the caller, a Media Foundation thread.
func setWebcam(v bool) {
	if webcamActive.Swap(v) == v {
		return
	}
	log.Println("Webcam in use:", v)
	select {
	case webcamChanged <- struct{}{}:
	default:
	}
}

// runWebcamPublisher publishes the current webcam state after every change.
func runWebcamPublisher() {
	for range webcamChanged {
		t, sent := enqueueWebcam(getClient())
		if t == nil {
			continue
		}
		logWebcam(awaitToken(t, time.Now().Add(powerPublishTimeout)), sent)
	}
}
