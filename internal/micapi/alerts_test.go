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
	set    map[string]mic.Alert
	passes chan struct{}
}

func (f *fakeAlertStore) RecorderStatuses(context.Context) ([]mic.RecorderStatus, error) {
	if f.passes != nil {
		f.passes <- struct{}{}
	}

	return f.rows, f.listEr
}

func (f *fakeAlertStore) SetRecorderAlert(_ context.Context, name string, a mic.Alert) error {
	if f.setEr != nil {
		return f.setEr
	}
	if f.set == nil {
		f.set = map[string]mic.Alert{}
	}
	f.set[name] = a

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
		{Name: "bed", LastSeen: alertNowFn(), ReceiverPresent: false, Alert: mic.AlertOK},
	}}
	rep := &fakeReplier{}
	if err := newAlertLoop(st, rep).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rep.sends) != 1 || rep.sends[0].chat != -1001234567890 || rep.sends[0].replyTo != 0 {
		t.Fatalf("sends = %+v", rep.sends)
	}
	if st.set["bed"] != mic.AlertMissingReceiver {
		t.Fatalf("stored = %v", st.set)
	}
}

func TestAlertLoopNoChangeSendsNothing(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "bed", LastSeen: alertNowFn(), ReceiverPresent: true, Alert: mic.AlertOK},
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
		{Name: "bed", LastSeen: alertNowFn().Add(-time.Hour), ReceiverPresent: true, Alert: mic.AlertOK},
	}}
	rep := &fakeReplier{sendErr: errors.New("telegram down")}
	if err := newAlertLoop(st, rep).Pass(t.Context()); err == nil {
		t.Fatal("want error")
	}
	if len(st.set) != 0 {
		t.Fatalf("stored despite failed send: %v", st.set)
	}
	// The next pass retries.
	rep.sendErr = nil
	if err := newAlertLoop(st, rep).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st.set["bed"] != mic.AlertSilent || len(rep.sends) != 1 {
		t.Fatalf("set=%v sends=%v", st.set, rep.sends)
	}
}

func TestAlertLoopOneFailureDoesNotBlockOthers(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{
		{Name: "a", LastSeen: alertNowFn(), ReceiverPresent: false, Alert: mic.AlertOK},
		{Name: "b", LastSeen: alertNowFn(), StorageFull: true, ReceiverPresent: true, Alert: mic.AlertOK},
	}, setEr: errors.New("disk")}
	rep := &fakeReplier{}
	err := newAlertLoop(st, rep).Pass(t.Context())
	if err == nil || len(rep.sends) != 2 {
		t.Fatalf("err=%v sends=%v", err, rep.sends)
	}
}

func TestAlertLoopSkipsRecorderNeverSeen(t *testing.T) {
	st := &fakeAlertStore{rows: []mic.RecorderStatus{{Name: "ghost", Alert: mic.AlertOK}}}
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
