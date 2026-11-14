package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Event is one new message seen on the update stream.
type Event struct {
	Seq      int64  `json:"seq"`
	ChatKind string `json:"chat_kind"`
	Message
}

// EventLog is a bounded, in-memory log of new messages with monotonically
// increasing sequence numbers, so clients can long-poll without missing or
// double-reading. It does not survive restarts: the boot ID changes, and a
// client that sees a different boot must re-read its chats with get_messages.
type EventLog struct {
	boot string
	cap  int

	mu     sync.Mutex
	seq    int64
	buf    []Event       // oldest first, len <= cap
	notify chan struct{} // closed and replaced on every append
}

func NewEventLog(capacity int) *EventLog {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return &EventLog{boot: hex.EncodeToString(b), cap: capacity, notify: make(chan struct{})}
}

func (l *EventLog) Append(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	if len(l.buf) == l.cap {
		copy(l.buf, l.buf[1:])
		l.buf = l.buf[:len(l.buf)-1]
	}
	l.buf = append(l.buf, e)
	close(l.notify)
	l.notify = make(chan struct{})
}

// WaitResult is what wait_for_messages returns.
type WaitResult struct {
	Boot    string  `json:"boot" jsonschema:"server boot ID; pass it back on the next call"`
	LastSeq int64   `json:"last_seq" jsonschema:"pass back as after_seq on the next call"`
	Events  []Event `json:"events"`
	Reset   bool    `json:"reset,omitempty" jsonschema:"the server restarted since your boot ID: re-read the chats you follow with get_messages, then continue from last_seq"`
	Gap     bool    `json:"gap,omitempty" jsonschema:"events after your after_seq were dropped from the buffer: re-read the chats you follow with get_messages"`
}

// Wait returns events with seq > after that match keep, blocking until at
// least one arrives, the timeout passes, or ctx ends. after < 0 means "from now".
// LastSeq always advances past everything scanned, matching or not, so a
// filtered caller never rescans the same events.
func (l *EventLog) Wait(ctx context.Context, boot string, after int64, timeout time.Duration, limit int, keep func(Event) bool) WaitResult {
	l.mu.Lock()
	if boot != "" && boot != l.boot {
		res := WaitResult{Boot: l.boot, LastSeq: l.seq, Events: []Event{}, Reset: true}
		l.mu.Unlock()
		return res
	}
	if after < 0 || after > l.seq {
		after = l.seq
	}
	l.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		l.mu.Lock()
		res := WaitResult{Boot: l.boot, LastSeq: after, Events: []Event{}}
		if len(l.buf) > 0 && l.buf[0].Seq > after+1 {
			res.Gap = true
		}
		for _, e := range l.buf {
			if e.Seq <= after {
				continue
			}
			res.LastSeq = e.Seq
			if keep(e) {
				res.Events = append(res.Events, e)
				if len(res.Events) == limit {
					break
				}
			}
		}
		after = res.LastSeq
		ch := l.notify
		l.mu.Unlock()

		if len(res.Events) > 0 || res.Gap {
			return res
		}
		select {
		case <-ch:
		case <-timer.C:
			return res
		case <-ctx.Done():
			return res
		}
	}
}
