package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
)

// ErrDuplicate means the capture's source identity (source, chat, message)
// is already stored: the message was delivered more than once (FR-006).
var ErrDuplicate = errors.New("pipeline: duplicate capture")

// Inserter is the slice of the capture queue ingest needs. Insert stores c
// and sets c.ID. A duplicate source identity must yield an error whose chain
// holds a value with a Duplicate() bool method returning true, so the
// pipeline recognises it without importing the store.
type Inserter interface {
	Insert(ctx context.Context, c *capture.Capture) error
}

// FileAppender is the slice of the capture queue file ingest needs.
// AppendFile stores item, a files capture at collect holding one file, by
// joining the chat's open batch or starting one; it owns the batch deadline
// (capture.JoinBatch with quiet). It reports whether a capture was created
// and whether the batch deadline moved later. Duplicates are signalled like
// Inserter's.
type FileAppender interface {
	AppendFile(
		ctx context.Context,
		item capture.Capture,
		quiet time.Duration,
	) (capture.AppendResult, error)
	// JoinOpenBatch is AppendFile that never starts a batch: it stores
	// item only when it joins the chat's open batch, and otherwise stores
	// nothing and reports joined false.
	JoinOpenBatch(
		ctx context.Context,
		item capture.Capture,
		quiet time.Duration,
	) (res capture.AppendResult, joined bool, err error)
	// RestartQuiet pushes the collect deadline of batch id to now + quiet
	// when it is earlier and the batch is still at collect, and does
	// nothing otherwise. It must never pull a deadline in, and a deadline
	// move must make a concurrent AdvanceCollect of the batch fail.
	RestartQuiet(ctx context.Context, id int64, quiet time.Duration) error
	// SetFileLocalPath records that file position of capture id is held
	// locally under name (relative to the files directory). It changes
	// nothing else.
	SetFileLocalPath(ctx context.Context, id int64, position int, name string) error
}

// Waker is the worker as ingest sees it; *Worker implements it.
type Waker interface {
	// Wake says a new capture is queued.
	Wake()
	// HoldAck keeps the worker off the capture with c's source identity
	// until the returned release is called, or AckHoldLimit passes.
	HoldAck(c capture.Capture) (release func())
}

// Ack acknowledges a capture that is durably stored, for example by sending
// ⏳ and recording its message ID. It runs before the worker may process
// the capture, so the final reply edits that acknowledgement instead of
// racing it (FR-025). It cannot fail the ingest: the row is already stored.
type Ack func(ctx context.Context, stored capture.Capture)

// Ingester is the source-agnostic entry into the pipeline (FR-007): the
// Telegram adapter uses it today and a later HTTP route can call it too.
type Ingester struct {
	store Inserter
	waker Waker

	files FileAppender
	quiet time.Duration
	// fetch and held keep a batch's first item locally before its ack.
	fetch Downloader
	held  FileStore
	log   *zap.Logger
}

// IngesterOption configures optional ingest features.
type IngesterOption func(*Ingester)

// WithFiles enables IngestFile: items are appended through files, which
// applies quiet as the batch quiet period. The item that creates a batch is
// fetched with fetch and written to held before its ack (Constitution VI).
func WithFiles(files FileAppender, quiet time.Duration, fetch Downloader, held FileStore) IngesterOption {
	return func(i *Ingester) { i.files, i.quiet, i.fetch, i.held = files, quiet, fetch, held }
}

// WithLog sets the logger for ingest problems that cannot fail an ingest
// (default: none).
func WithLog(log *zap.Logger) IngesterOption {
	return func(i *Ingester) {
		if log != nil {
			i.log = log
		}
	}
}

// NewIngester returns an Ingester inserting into store and waking waker.
func NewIngester(store Inserter, waker Waker, opts ...IngesterOption) (*Ingester, error) {
	if store == nil {
		return nil, errors.New("pipeline: nil store")
	}
	if waker == nil {
		return nil, errors.New("pipeline: nil waker")
	}

	i := &Ingester{store: store, waker: waker, log: zap.NewNop()}
	for _, opt := range opts {
		opt(i)
	}
	if i.files != nil && (i.fetch == nil || i.held == nil) {
		return nil, errors.New("pipeline: file ingest needs a downloader and a file store")
	}

	return i, nil
}

// Ingest durably inserts c, runs ack (when not nil) and then wakes the
// worker. It returns the stored capture with its ID. A redelivered capture
// returns ErrDuplicate; any other store error is wrapped and returned. In
// both cases ack does not run and the worker is not woken. Callers
// acknowledge only through ack, after the insert (Constitution VI).
//
// While ack runs the worker is held off the capture. The hold starts before
// the insert, so no scan can see the row without it, and it lives only in
// memory: a restart drops it with nothing left to wait for.
func (i *Ingester) Ingest(ctx context.Context, c capture.Capture, ack Ack) (capture.Capture, error) {
	stored, err := i.insertAndAck(ctx, c, ack)
	if err != nil {
		return capture.Capture{}, err
	}
	i.waker.Wake()

	return stored, nil
}

func (i *Ingester) insertAndAck(ctx context.Context, c capture.Capture, ack Ack) (capture.Capture, error) {
	return i.storeAndAck(ctx, c, ack, func() (capture.Capture, bool, error) {
		if err := i.store.Insert(ctx, &c); err != nil {
			return capture.Capture{}, false, fmt.Errorf("insert: %w", err)
		}

		return c, true, nil
	}, nil)
}

// storeAndAck holds the worker off c while store runs and, when it created
// a capture, acks it and then runs afterAck (when not nil), still under the
// hold. A duplicate maps to ErrDuplicate.
func (i *Ingester) storeAndAck(
	ctx context.Context, c capture.Capture, ack Ack, store func() (capture.Capture, bool, error),
	afterAck func(ctx context.Context, stored capture.Capture),
) (capture.Capture, error) {
	if ack != nil {
		release := i.waker.HoldAck(c)
		defer release()
	}
	stored, created, err := store()
	if err != nil {
		if isDuplicate(err) {
			return capture.Capture{}, ErrDuplicate
		}

		return capture.Capture{}, fmt.Errorf("pipeline: ingest: %w", err)
	}
	if created && ack != nil {
		ack(ctx, stored)
		if afterAck != nil {
			afterAck(ctx, stored)
		}
	}

	return stored, nil
}

// duplicate is the behaviour a store error exposes for a duplicate insert.
type duplicate interface {
	error
	Duplicate() bool
}

func isDuplicate(err error) bool {
	d, ok := errors.AsType[duplicate](err)

	return ok && d.Duplicate()
}
