// Package micapi holds the service side of the microphone HTTP API. It is
// imported by the service, so it must stay free of cgo.
package micapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

const (
	// sendTimeout bounds the Telegram send of a start notice.
	sendTimeout = 10 * time.Second
	// textRecording is the silent start message (contracts/telegram-ux.md).
	textRecording = "🎙 recording…"
	// tombstoneTTL is how long a discard is remembered so that a start
	// notice arriving late (or still sending) is withdrawn.
	tombstoneTTL = 5 * time.Minute
)

// StartStore keeps the start message of each recording until its upload
// takes it. *sqlite.DB is the production adapter.
type StartStore interface {
	PutStartReply(ctx context.Context, recordingID, chatID, messageID int64) error
	TakeStartReply(ctx context.Context, recordingID int64) (chatID, messageID int64, ok bool, err error)
	HasStartReply(ctx context.Context, recordingID int64) (bool, error)
	// HasRecording reports whether the recording was already ingested.
	HasRecording(ctx context.Context, chatID, recordingID int64) (bool, error)
}

// Starts sends and withdraws the "recording..." notices of microphone
// recordings. Both calls are best-effort and idempotent.
type Starts struct {
	store   StartStore
	replier pipeline.Replier
	chat    int64
	timeout time.Duration

	mu       sync.Mutex
	inflight map[int64]struct{}
	// discarded remembers when each recording was discarded.
	discarded map[int64]time.Time
	now       func() time.Time
}

// NewStarts builds Starts sending to the notify chat.
func NewStarts(store StartStore, replier pipeline.Replier, notifyChat int64) *Starts {
	return &Starts{
		store:     store,
		replier:   replier,
		chat:      notifyChat,
		timeout:   sendTimeout,
		inflight:  map[int64]struct{}{},
		discarded: map[int64]time.Time{},
		now:       time.Now,
	}
}

// SetClock overrides the clock (for tests).
func (s *Starts) SetClock(now func() time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }

// SetTimeout overrides the send timeout (for tests).
func (s *Starts) SetTimeout(d time.Duration) { s.timeout = d }

// Started sends the silent start notice for a recording and stores its
// message ID. A repeat for the same recording sends nothing. startedAt is
// accepted for the API contract; the notice does not show it.
func (s *Starts) Started(ctx context.Context, id int64, _ time.Time) error {
	if !s.claim(id) {
		return nil
	}
	defer s.release(id)

	if s.wasDiscarded(id) {
		return nil
	}
	has, err := s.store.HasStartReply(ctx, id)
	if err != nil {
		return fmt.Errorf("micapi: look up start notice of recording %d: %w", id, err)
	}
	if has {
		return nil
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	msgID, err := s.replier.Send(sendCtx, s.chat, 0, textRecording)
	if err != nil {
		return fmt.Errorf("micapi: send start notice of recording %d: %w", id, err)
	}
	if err = s.store.PutStartReply(ctx, id, s.chat, msgID); err != nil {
		return fmt.Errorf("micapi: store start notice of recording %d: %w", id, err)
	}

	// The notice is stale when the recording was discarded or already
	// ingested while it was sending. The row was stored first, so whichever
	// of this call, a discard or an upload takes it removes the message once.
	stale := s.wasDiscarded(id)
	if !stale {
		if stale, err = s.store.HasRecording(ctx, s.chat, id); err != nil {
			return fmt.Errorf("micapi: look up recording %d: %w", id, err)
		}
	}
	if stale {
		return s.withdraw(ctx, id)
	}

	return nil
}

// Discarded removes the start notice of a recording that was too short. A
// missing notice is not an error, nor is a message Telegram no longer has.
func (s *Starts) Discarded(ctx context.Context, id int64) error {
	s.markDiscarded(id)

	return s.withdraw(ctx, id)
}

// withdraw takes the recording's start notice, if any, and deletes the
// message.
func (s *Starts) withdraw(ctx context.Context, id int64) error {
	chat, msgID, ok, err := s.store.TakeStartReply(ctx, id)
	if err != nil {
		return fmt.Errorf("micapi: take start notice of recording %d: %w", id, err)
	}
	if !ok {
		return nil
	}
	if err = s.replier.Delete(ctx, chat, msgID); err != nil && !errors.Is(err, pipeline.ErrRejected) {
		return fmt.Errorf("micapi: delete start notice of recording %d: %w", id, err)
	}

	return nil
}

// claim marks the recording as being announced; false means a concurrent
// call is already doing it.
func (s *Starts) claim(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.inflight[id]; busy {
		return false
	}
	s.inflight[id] = struct{}{}

	return true
}

func (s *Starts) release(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

// markDiscarded remembers the discard of a recording and forgets old ones.
func (s *Starts) markDiscarded(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, at := range s.discarded {
		if now.Sub(at) > tombstoneTTL {
			delete(s.discarded, k)
		}
	}
	s.discarded[id] = now
}

// wasDiscarded reports whether the recording was discarded recently.
func (s *Starts) wasDiscarded(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.discarded[id]

	return ok && s.now().Sub(at) <= tombstoneTTL
}
