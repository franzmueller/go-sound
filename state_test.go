// Copyright (c) 2026 InfAI (CC SES)

package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBirthState(t *testing.T) {
	cases := []struct {
		locked, suspended bool
		want              string
	}{
		{false, false, "online"},
		{true, false, "offline"},
		{false, true, "offline"},
		{true, true, "offline"},
	}
	for _, c := range cases {
		if got := birthState(c.locked, c.suspended); got != c.want {
			t.Errorf("birthState(%v,%v)=%q want %q", c.locked, c.suspended, got, c.want)
		}
	}
}

func TestPresenceUnlockWhileSuspended(t *testing.T) {
	var p presence
	p.setLocked(true)
	p.setSuspended(true)
	if !p.setLocked(false) {
		t.Fatal("unlock should report a change")
	}
	if p.state() != "offline" {
		t.Fatal("unlock while suspended must stay offline")
	}
	if p.setSuspended(true) {
		t.Fatal("second suspend must be idempotent")
	}
	p.setSuspended(false)
	if p.state() != "online" {
		t.Fatal("resume while unlocked must be online")
	}
}

func TestBirthPayloadEscapes(t *testing.T) {
	b, err := birthPayload("dev_id", `My "PC" \ x`, "urn:t", "online")
	if err != nil {
		t.Fatal(err)
	}
	var m birthMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if m.Method != "set" || m.DeviceID != "dev_id" || m.Data.Name != `My "PC" \ x` || m.Data.State != "online" || m.Data.DeviceType != "urn:t" {
		t.Fatalf("unexpected: %+v", m)
	}
}

func TestPlayingSuppressed(t *testing.T) {
	if playingSuppressed(false) || !playingSuppressed(true) {
		t.Fatal("only suspend suppresses playing")
	}
	var p presence
	p.setLocked(true)
	if p.suppressed() {
		t.Fatal("lock must not suppress playing")
	}
	p.setSuspended(true)
	if !p.suppressed() {
		t.Fatal("suspend must suppress playing")
	}
}

func TestSilenceTimeout(t *testing.T) {
	if got := silenceTimeout(30*time.Minute, false); got != 30*time.Minute {
		t.Errorf("unlocked: got %v", got)
	}
	if got := silenceTimeout(30*time.Minute, true); got != 0 {
		t.Errorf("locked: got %v", got)
	}
}
