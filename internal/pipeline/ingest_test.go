package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
)

// dupError mimics a store's duplicate error: it reports Duplicate() true
// without the pipeline importing the store.
type dupError struct{}

func (dupError) Error() string   { return "fake store: duplicate capture" }
func (dupError) Duplicate() bool { return true }

// fakeInserter records inserts and assigns IDs; err, when set, is returned
// instead of inserting.
type fakeInserter struct {
	nextID int64
	rows   []capture.Capture
	err    error
	// wakesAtInsert records the Wake count seen at each insert, to prove the order.
	waker         *fakeWaker
	wakesAtInsert []int
}

func (f *fakeInserter) Insert(_ context.Context, c *capture.Capture) error {
	if f.waker != nil {
		f.wakesAtInsert = append(f.wakesAtInsert, f.waker.n)
	}
	if f.err != nil {
		return f.err
	}
	f.nextID++
	c.ID = f.nextID
	f.rows = append(f.rows, *c)

	return nil
}

// fakeWaker counts wakes and, when log is set, records the order of holds,
// releases and wakes.
type fakeWaker struct {
	n   int
	log *[]string
}

func (w *fakeWaker) Wake() {
	w.n++
	w.note("wake")
}

func (w *fakeWaker) HoldAck(c capture.Capture) func() {
	w.note(fmt.Sprintf("hold %s/%d/%d", c.Source, c.SourceChatID, c.SourceMessageID))

	return func() { w.note("release") }
}

func (w *fakeWaker) note(e string) {
	if w.log != nil {
		*w.log = append(*w.log, e)
	}
}

func TestNewIngesterRejectsNil(t *testing.T) {
	if _, err := pipeline.NewIngester(nil, &fakeWaker{}); err == nil {
		t.Fatal("nil store: want error")
	}
	if _, err := pipeline.NewIngester(&fakeInserter{}, nil); err == nil {
		t.Fatal("nil waker: want error")
	}
}

func TestIngestInsertsThenWakes(t *testing.T) {
	w := &fakeWaker{}
	st := &fakeInserter{nextID: 41, waker: w}
	in, err := pipeline.NewIngester(st, w)
	if err != nil {
		t.Fatal(err)
	}

	c := textCapture(t, 0)
	c.Text = "hello"
	got, err := in.Ingest(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 42 || got.Text != "hello" {
		t.Fatalf("stored capture = %+v, want ID 42 with text", got)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(st.rows))
	}
	if st.wakesAtInsert[0] != 0 || w.n != 1 {
		t.Fatalf("wake before insert or missing: at insert %v, total %d", st.wakesAtInsert, w.n)
	}
}

func TestIngestDuplicate(t *testing.T) {
	w := &fakeWaker{}
	in, err := pipeline.NewIngester(&fakeInserter{err: dupError{}}, w)
	if err != nil {
		t.Fatal(err)
	}

	_, err = in.Ingest(context.Background(), textCapture(t, 0), nil)
	if !errors.Is(err, pipeline.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if w.n != 0 {
		t.Fatalf("duplicate must not wake, woke %d", w.n)
	}
}

func TestIngestStoreError(t *testing.T) {
	w := &fakeWaker{}
	in, err := pipeline.NewIngester(&fakeInserter{err: errStore}, w)
	if err != nil {
		t.Fatal(err)
	}

	_, err = in.Ingest(context.Background(), textCapture(t, 0), nil)
	if !errors.Is(err, errStore) || errors.Is(err, pipeline.ErrDuplicate) {
		t.Fatalf("want wrapped store error, got %v", err)
	}
	if w.n != 0 {
		t.Fatalf("failed insert must not wake, woke %d", w.n)
	}
}

// orderInserter records the insert in the shared event log.
type orderInserter struct {
	log *[]string
	err error
}

func (o *orderInserter) Insert(_ context.Context, c *capture.Capture) error {
	*o.log = append(*o.log, "insert")
	if o.err != nil {
		return o.err
	}
	c.ID = 42

	return nil
}

func ackedCapture(t *testing.T) capture.Capture {
	t.Helper()

	c := textCapture(t, 0)
	c.Source, c.SourceChatID, c.SourceMessageID = "telegram", -1001234567890, 7

	return c
}

// The worker is held off the capture from before its row exists until the
// acknowledgement is done, and is woken only after that (FR-025).
func TestIngestAcksBetweenInsertAndWake(t *testing.T) {
	var log []string
	w := &fakeWaker{log: &log}
	in, err := pipeline.NewIngester(&orderInserter{log: &log}, w)
	if err != nil {
		t.Fatal(err)
	}

	got, err := in.Ingest(context.Background(), ackedCapture(t), func(_ context.Context, c capture.Capture) {
		log = append(log, fmt.Sprintf("ack %d", c.ID))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hold telegram/-1001234567890/7", "insert", "ack 42", "release", "wake"}
	if !slices.Equal(log, want) {
		t.Fatalf("order %q, want %q", log, want)
	}
	if got.ID != 42 {
		t.Fatalf("ID = %d, want 42", got.ID)
	}
}

// A failed insert releases the hold and never acknowledges.
func TestIngestFailureReleasesHold(t *testing.T) {
	for name, insertErr := range map[string]error{"duplicate": dupError{}, "store": errStore} {
		var log []string
		w := &fakeWaker{log: &log}
		in, err := pipeline.NewIngester(&orderInserter{log: &log, err: insertErr}, w)
		if err != nil {
			t.Fatal(err)
		}

		_, err = in.Ingest(context.Background(), ackedCapture(t), func(context.Context, capture.Capture) {
			log = append(log, "ack")
		})
		if err == nil {
			t.Fatalf("%s: want error", name)
		}
		if want := []string{"hold telegram/-1001234567890/7", "insert", "release"}; !slices.Equal(log, want) {
			t.Fatalf("%s: order %q, want %q", name, log, want)
		}
	}
}

// Without an acknowledgement there is nothing to wait for: no hold.
func TestIngestWithoutAckTakesNoHold(t *testing.T) {
	var log []string
	w := &fakeWaker{log: &log}
	in, err := pipeline.NewIngester(&orderInserter{log: &log}, w)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := in.Ingest(context.Background(), ackedCapture(t), nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"insert", "wake"}; !slices.Equal(log, want) {
		t.Fatalf("order %q, want %q", log, want)
	}
}
