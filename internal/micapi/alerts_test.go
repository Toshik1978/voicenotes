package micapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/mic"
	"github.com/Toshik1978/voicenotes/internal/micapi"
)

type fakeAlertStore struct {
	rows   []mic.RecorderStatus
	listEr error
	setEr  error
	set    map[string]mic.AlertSet
	stores []mic.AlertSet
	passes chan struct{}
}

func (f *fakeAlertStore) RecorderStatuses(context.Context) ([]mic.RecorderStatus, error) {
	if f.passes != nil {
		f.passes <- struct{}{}
	}

	return f.rows, f.listEr
}

func (f *fakeAlertStore) SetRecorderAlert(_ context.Context, name string, a mic.AlertSet) error {
	if f.setEr != nil {
		return f.setEr
	}
	if f.set == nil {
		f.set = map[string]mic.AlertSet{}
	}
	f.set[name] = a
	f.stores = append(f.stores, a)

	return nil
}

func alertNowFn() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

func newAlertLoop(st *fakeAlertStore, rep *fakeReplier) *micapi.AlertLoop {
	l := micapi.NewAlertLoop(st, rep, -1001234567890, 5*time.Minute, time.UTC)
	l.SetClock(alertNowFn)

	return l
}

func TestAlertLoopSendsThenStores(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn(), ReceiverPresent: false},
	}}
	rep := &fakeReplier{}
	if err := newAlertLoop(st, rep).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rep.sends) != 1 || rep.sends[0].chat != -1001234567890 || rep.sends[0].replyTo != 0 {
		t.Fatalf("sends = %+v", rep.sends)
	}
	if st.set["bed"] != mic.NewAlertSet(mic.AlertMissingReceiver) {
		t.Fatalf("stored = %v", st.set)
	}
}

func TestAlertLoopNoChangeSendsNothing(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn(), ReceiverPresent: true},
	}}
	rep := &fakeReplier{}
	if err := newAlertLoop(st, rep).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rep.sends) != 0 || len(st.set) != 0 {
		t.Fatalf("sends=%v set=%v", rep.sends, st.set)
	}
}

func TestAlertLoopSendFailureKeepsStoredAlert(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn().Add(-time.Hour), ReceiverPresent: true},
	}}
	rep := &fakeReplier{sendErr: errors.New("telegram down")}
	l := newAlertLoop(st, rep)
	rows := st.rows
	st.rows = nil
	l.SetClock(func() time.Time { return alertNowFn().Add(-10 * time.Minute) })
	if err := l.Pass(t.Context()); err != nil { // service started 10 minutes ago
		t.Fatal(err)
	}
	st.rows = rows
	l.SetClock(alertNowFn)
	if err := l.Pass(t.Context()); err == nil {
		t.Fatal("want error")
	}
	if len(st.set) != 0 {
		t.Fatalf("stored despite failed send: %v", st.set)
	}
	// The next pass retries.
	rep.sendErr = nil
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st.set["bed"] != mic.NewAlertSet(mic.AlertSilent) || len(rep.sends) != 1 {
		t.Fatalf("set=%v sends=%v", st.set, rep.sends)
	}
}

func TestAlertLoopOneFailureDoesNotBlockOthers(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "a", LastSeen: alertNowFn(), ReceiverPresent: false},
		{Name: "b", LastSeen: alertNowFn(), StorageFull: true, ReceiverPresent: true},
	}, setEr: errors.New("disk")}
	rep := &fakeReplier{}
	err := newAlertLoop(st, rep).Pass(t.Context())
	if err == nil || len(rep.sends) != 2 {
		t.Fatalf("err=%v sends=%v", err, rep.sends)
	}
}

func TestAlertLoopSkipsRecorderNeverSeen(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{{Name: "ghost"}}}
	rep := &fakeReplier{}
	if err := newAlertLoop(st, rep).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rep.sends) != 0 || len(st.set) != 0 {
		t.Fatalf("sends=%v set=%v", rep.sends, st.set)
	}
}

func TestAlertLoopListError(t *testing.T) {
	st := &fakeAlertStore{listEr: errors.New("db")}
	if err := newAlertLoop(st, &fakeReplier{}).Pass(t.Context()); err == nil {
		t.Fatal("want error")
	}
}

func TestAlertLoopRunPassesOnStartAndEachTick(t *testing.T) {
	st := &fakeAlertStore{passes: make(chan struct{})}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { newAlertLoop(st, &fakeReplier{}).RunWith(ctx, ticks); close(done) }()

	<-st.passes // initial pass

	ticks <- alertNowFn()
	<-st.passes // tick pass
	cancel()
	<-done
}

// Two changes in one pass: each message is sent and its set stored before
// the next, so a crash between them repeats at most one message.
func TestAlertLoopStoresAfterEachMessage(t *testing.T) {
	missing := mic.NewAlertSet(mic.AlertMissingReceiver)
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn(), ReceiverPresent: true, StorageFull: true, Outstanding: missing},
	}}
	rep := &fakeReplier{}
	if err := newAlertLoop(st, rep).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	full := mic.NewAlertSet(mic.AlertStorageFull)
	if len(rep.sends) != 2 || len(st.stores) != 2 ||
		st.stores[0] != missing|full || st.stores[1] != full {
		t.Fatalf("sends=%v stores=%v", rep.sends, st.stores)
	}
}

func TestAlertLoopFailedSecondSendKeepsFirstStored(t *testing.T) {
	missing := mic.NewAlertSet(mic.AlertMissingReceiver)
	full := mic.NewAlertSet(mic.AlertStorageFull)
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn(), ReceiverPresent: true, StorageFull: true, Outstanding: missing},
	}}
	rep := &fakeReplier{failAfter: 1, sendErr: errors.New("telegram down")}
	if err := newAlertLoop(st, rep).Pass(t.Context()); err == nil {
		t.Fatal("want error")
	}
	if len(st.stores) != 1 || st.stores[0] != missing|full {
		t.Fatalf("stores=%v, want only the first step stored", st.stores)
	}
}

// clockAt returns a settable clock.
func clockAt(t time.Time) (func() time.Time, func(time.Duration)) {
	return func() time.Time { return t }, func(d time.Duration) { t = t.Add(d) }
}

func TestAlertLoopRestartDoesNotAlertStaleRecorder(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn().Add(-3 * time.Hour), ReceiverPresent: true},
	}}
	rep := &fakeReplier{}
	l := newAlertLoop(st, rep)
	now, advance := clockAt(alertNowFn())
	l.SetClock(now)
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	advance(4 * time.Minute)
	// Checks in within the window: never silent, no alert, no recovery.
	st.rows[0].LastSeen = now()
	advance(time.Minute)
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rep.sends) != 0 || len(st.set) != 0 {
		t.Fatalf("sends=%v set=%v", rep.sends, st.set)
	}
}

func TestAlertLoopRestartNeverCheckingInAlertsAfterSilenceWindow(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn().Add(-3 * time.Hour), ReceiverPresent: true},
	}}
	rep := &fakeReplier{}
	l := newAlertLoop(st, rep)
	now, advance := clockAt(alertNowFn())
	l.SetClock(now)
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	advance(5 * time.Minute)
	if err := l.Pass(t.Context()); err != nil || len(rep.sends) != 0 {
		t.Fatalf("err=%v sends=%v", err, rep.sends)
	}
	advance(time.Second)
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rep.sends) != 1 || st.set["bed"] != mic.NewAlertSet(mic.AlertSilent) {
		t.Fatalf("sends=%v set=%v", rep.sends, st.set)
	}
}

func TestAlertLoopRestartKeepsOutstandingSilentUntilCheckIn(t *testing.T) {
	silent := mic.NewAlertSet(mic.AlertSilent)
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn().Add(-3 * time.Hour), ReceiverPresent: true, Outstanding: silent},
	}}
	rep := &fakeReplier{}
	l := newAlertLoop(st, rep)
	now, advance := clockAt(alertNowFn())
	l.SetClock(now)
	for range 3 {
		if err := l.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		advance(time.Minute)
	}
	if len(rep.sends) != 0 || len(st.set) != 0 {
		t.Fatalf("spurious: sends=%v set=%v", rep.sends, st.set)
	}
	// Its first check-in after the restart clears the condition.
	st.rows[0].LastSeen = now()
	if err := l.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rep.sends) != 1 || st.set["bed"] != 0 {
		t.Fatalf("sends=%v set=%v", rep.sends, st.set)
	}
}
