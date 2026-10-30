package telegram_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/telegram"
)

const token = "123456:FAKE-TOKEN"

// call is one Bot API request seen by the fake server.
type apiCall struct {
	method string
	params map[string]any
}

// fakeAPI is a minimal Telegram Bot API. respond decides the reply for each
// method; unknown methods get ok:true with an empty result.
type fakeAPI struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []apiCall
	respond map[string]func(params map[string]any) (int, string)
	files   map[string][]byte
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	f := &fakeAPI{t: t, respond: map[string]func(map[string]any) (int, string){}, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if path, ok := strings.CutPrefix(r.URL.Path, "/file/bot"+token+"/"); ok {
		data, found := f.files[path]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)

		return
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+token+"/")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	params := map[string]any{}
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		if err := json.Unmarshal(body, &params); err != nil {
			f.t.Errorf("decode %s: %v", method, err)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, apiCall{method, params})
	h := f.respond[method]
	f.mu.Unlock()

	code, out := http.StatusOK, `{"ok":true,"result":true}`
	if h != nil {
		code, out = h(params)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, out)
}

func (f *fakeAPI) on(method string, h func(params map[string]any) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond[method] = h
}

func (f *fakeAPI) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.method)
	}

	return out
}

func (f *fakeAPI) call(t *testing.T, method string) map[string]any {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == method {
			return c.params
		}
	}
	t.Fatalf("no %s call; calls: %v", method, f.calls)

	return nil
}

func message(id int) func(map[string]any) (int, string) {
	return func(map[string]any) (int, string) {
		return http.StatusOK, fmt.Sprintf(
			`{"ok":true,"result":{"message_id":%d,"date":1,"chat":{"id":%d,"type":"private"}}}`, id, chat,
		)
	}
}

func apiError(code int, desc string) func(map[string]any) (int, string) {
	return func(map[string]any) (int, string) {
		return code, fmt.Sprintf(`{"ok":false,"error_code":%d,"description":%q}`, code, desc)
	}
}

func newAPI(t *testing.T, f *fakeAPI) *telegram.API {
	t.Helper()

	a, err := telegram.NewAPI(telegram.APIConfig{
		Token: token, URL: f.srv.URL, PollTimeout: 50 * time.Millisecond, Log: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}

	return a
}

func wantSilentHTML(t *testing.T, p map[string]any) {
	t.Helper()

	if p["disable_notification"] != "true" || p["parse_mode"] != "HTML" {
		t.Fatalf("not silent HTML: %v", p)
	}
}

func TestNewAPIValidates(t *testing.T) {
	if _, err := telegram.NewAPI(telegram.APIConfig{}); err == nil {
		t.Fatal("empty token: want error")
	}
}

func TestSendIsSilentReply(t *testing.T) {
	f := newFakeAPI(t)
	f.on("sendMessage", message(501))
	a := newAPI(t, f)

	id, err := a.Send(context.Background(), chat, 42, "⏳")
	if err != nil || id != 501 {
		t.Fatalf("Send = %d, %v", id, err)
	}
	p := f.call(t, "sendMessage")
	wantSilentHTML(t, p)
	if p["chat_id"] != strconv.FormatInt(chat, 10) || p["text"] != "⏳" || p["reply_to_message_id"] != "42" ||
		p["allow_sending_without_reply"] != "true" {
		t.Fatalf("params %v", p)
	}
}

func TestSendErrors(t *testing.T) {
	f := newFakeAPI(t)
	f.on("sendMessage", apiError(http.StatusForbidden, "Forbidden: bot was blocked by the user"))
	a := newAPI(t, f)

	if _, err := a.Send(context.Background(), chat, 0, "x"); err == nil || strings.Contains(err.Error(), "FAKE-TOKEN") {
		t.Fatalf("want token-free error, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Send(ctx, chat, 0, "x"); err == nil {
		t.Fatal("cancelled ctx: want error")
	}
}

func TestEdit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(map[string]any) (int, string)
		wantID  int64
		wantErr bool
		methods []string
	}{
		{"edited", message(77), 77, false, []string{"editMessageText"}},
		{
			"not modified", apiError(http.StatusBadRequest, "Bad Request: message is not modified"),
			77, false,
			[]string{"editMessageText"},
		},
		{
			"deleted falls back", apiError(http.StatusBadRequest, "Bad Request: message to edit not found"),
			601, false,
			[]string{"editMessageText", "sendMessage"},
		},
		{
			"too old falls back", apiError(http.StatusBadRequest, "Bad Request: message can't be edited"),
			601, false,
			[]string{"editMessageText", "sendMessage"},
		},
		{
			"server error is retried later", apiError(http.StatusInternalServerError, "Internal Server Error"),
			0, true,
			[]string{"editMessageText"},
		},
		{
			"flood is retried later", func(map[string]any) (int, string) {
				return http.StatusTooManyRequests,
					`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 5","parameters":{"retry_after":5}}`
			},
			0, true,
			[]string{"editMessageText"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.on("editMessageText", tc.edit)
			f.on("sendMessage", message(601))
			a := newAPI(t, f)

			id, err := a.Edit(context.Background(), chat, 77, "✓ saved")
			if (err != nil) != tc.wantErr || id != tc.wantID {
				t.Fatalf("Edit = %d, %v", id, err)
			}
			if got := f.methods(); strings.Join(got, ",") != strings.Join(tc.methods, ",") {
				t.Fatalf("methods %v, want %v", got, tc.methods)
			}
			p := f.call(t, "editMessageText")
			wantSilentHTML(t, p)
			if p["message_id"] != "77" || p["chat_id"] != strconv.FormatInt(chat, 10) || p["text"] != "✓ saved" {
				t.Fatalf("edit params %v", p)
			}
			if len(tc.methods) == 2 {
				s := f.call(t, "sendMessage")
				wantSilentHTML(t, s)
				if s["text"] != "✓ saved" || s["allow_sending_without_reply"] != "true" {
					t.Fatalf("fallback params %v", s)
				}
			}
		})
	}
}

func TestDownload(t *testing.T) {
	f := newFakeAPI(t)
	f.on("getFile", func(p map[string]any) (int, string) {
		return http.StatusOK, fmt.Sprintf(`{"ok":true,"result":{"file_id":%q,"file_path":"voice/%s.oga"}}`,
			p["file_id"], p["file_id"])
	})
	f.files["voice/ok.oga"] = []byte("OggS-fake")
	f.files["voice/big.oga"] = bytes.Repeat([]byte{1}, telegram.MaxFileSize+1)
	a := newAPI(t, f)
	ctx := context.Background()

	data, err := a.Download(ctx, "ok")
	if err != nil || string(data) != "OggS-fake" {
		t.Fatalf("Download = %q, %v", data, err)
	}
	if _, err := a.Download(ctx, "big"); !errors.Is(err, telegram.ErrTooLarge) {
		t.Fatalf("big: want ErrTooLarge, got %v", err)
	}
	if _, err := a.Download(ctx, "missing"); err == nil || errors.Is(err, telegram.ErrTooLarge) ||
		strings.Contains(err.Error(), "FAKE-TOKEN") {
		t.Fatalf("missing: want token-free error, got %v", err)
	}
	f.on("getFile", apiError(http.StatusBadRequest, "Bad Request: file is too big"))
	if _, err := a.Download(ctx, "huge"); !errors.Is(err, telegram.ErrTooLarge) {
		t.Fatalf("file is too big: want ErrTooLarge, got %v", err)
	}
	f.on("getFile", apiError(http.StatusBadRequest, "Bad Request: invalid file_id"))
	if _, err := a.Download(ctx, "bad"); err == nil {
		t.Fatal("getFile failure: want error")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.Download(cancelled, "ok"); err == nil {
		t.Fatal("cancelled ctx: want error")
	}
}

type recordingHandler struct {
	got chan telegram.Message
}

func (r *recordingHandler) Handle(_ context.Context, m telegram.Message) { r.got <- m }

func TestRunPollsMessagesOnly(t *testing.T) {
	f := newFakeAPI(t)
	var once sync.Once
	f.on("getUpdates", func(map[string]any) (int, string) {
		out := `{"ok":true,"result":[]}`
		once.Do(func() {
			out = fmt.Sprintf(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":42,"date":1798614900,`+
				`"chat":{"id":%d,"type":"private"},"from":{"id":%d,"is_bot":false,"first_name":"A"},"text":"hi"}}]}`,
				chat, owner)
		})
		time.Sleep(10 * time.Millisecond)

		return http.StatusOK, out
	})
	a := newAPI(t, f)
	h := &recordingHandler{got: make(chan telegram.Message, 1)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx, h)
		close(done)
	}()

	select {
	case m := <-h.got:
		if m.ChatID != chat || m.ID != 42 || m.SenderID != owner || m.Text != "hi" ||
			!m.Date.Equal(time.Unix(1798614900, 0)) {
			t.Fatalf("message %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never called")
	}
	p := f.call(t, "getUpdates")
	if au, _ := p["allowed_updates"].([]any); len(au) != 1 || au[0] != "message" {
		t.Fatalf("allowed_updates = %v", p["allowed_updates"])
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunRecoversFromPollErrorsAndMapsAudio(t *testing.T) {
	f := newFakeAPI(t)
	var mu sync.Mutex
	n := 0
	f.on("getUpdates", func(map[string]any) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		n++
		switch n {
		case 1, 2:
			return http.StatusBadGateway, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`
		case 3:
			return http.StatusOK, fmt.Sprintf(`{"ok":true,"result":[`+
				`{"update_id":5,"message":{"message_id":43,"date":1,"chat":{"id":%[1]d,"type":"private"},`+
				`"from":{"id":%[2]d,"is_bot":false,"first_name":"A"},"forward_origin":{"type":"hidden_user","date":1,`+
				`"sender_user_name":"B"},"voice":{"file_id":"V","file_unique_id":"u","duration":3,`+
				`"mime_type":"audio/ogg","file_size":1000}}},`+
				`{"update_id":6,"message":{"message_id":44,"date":1,"chat":{"id":%[1]d,"type":"private"},`+
				`"from":{"id":%[2]d,"is_bot":false,"first_name":"A"},"audio":{"file_id":"A","file_unique_id":"w",`+
				`"duration":3,"mime_type":"audio/mpeg","file_size":2000}}},`+
				`{"update_id":7,"message":{"message_id":45,"date":1,"chat":{"id":%[1]d,"type":"private"},`+
				`"from":{"id":%[2]d,"is_bot":false,"first_name":"A"},"sticker":{"file_id":"S","file_unique_id":"x",`+
				`"type":"regular","width":1,"height":1,"is_animated":false,"is_video":false}}}]}`, chat, owner)
		}
		time.Sleep(10 * time.Millisecond)

		return http.StatusOK, `{"ok":true,"result":[]}`
	})
	a, err := telegram.NewAPI(telegram.APIConfig{
		Token: token, URL: f.srv.URL, PollTimeout: time.Millisecond, RetryBase: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !a.LastPoll().IsZero() {
		t.Fatal("LastPoll before any poll must be zero")
	}
	h := &recordingHandler{got: make(chan telegram.Message, 3)}
	go a.Run(t.Context(), h)

	got := map[int64]telegram.Message{}
	for range 3 {
		select {
		case m := <-h.got:
			got[m.ID] = m
		case <-time.After(5 * time.Second):
			t.Fatalf("handler got only %d messages", len(got))
		}
	}
	if v := got[43].Audio; v == nil ||
		*v != (telegram.Audio{FileID: "V", MimeType: "audio/ogg", Size: 1000, Voice: true}) {
		t.Fatalf("forwarded voice = %+v", v)
	}
	if v := got[44].Audio; v == nil || *v != (telegram.Audio{FileID: "A", MimeType: "audio/mpeg", Size: 2000}) {
		t.Fatalf("audio = %+v", v)
	}
	if m := got[45]; m.Audio != nil || m.Text != "" {
		t.Fatalf("sticker must map to an empty (unsupported) message, got %+v", m)
	}
	if a.LastPoll().IsZero() {
		t.Fatal("LastPoll not recorded")
	}
	if p := f.call(t, "getUpdates"); p["allowed_updates"] == nil {
		t.Fatal("allowed_updates missing")
	}
}

// blockingHandler holds every message until release is closed.
type blockingHandler struct {
	started chan context.Context
	release chan struct{}
}

func (b *blockingHandler) Handle(ctx context.Context, _ telegram.Message) {
	b.started <- ctx
	<-b.release
}

// TestRunDrainsHandlersOnShutdown: an ingest in flight at shutdown must be
// able to commit, so Run returns only after it finishes and its context is
// not cancelled by the shutdown.
func TestRunDrainsHandlersOnShutdown(t *testing.T) {
	f := newFakeAPI(t)
	var once sync.Once
	f.on("getUpdates", func(map[string]any) (int, string) {
		out := `{"ok":true,"result":[]}`
		once.Do(func() {
			out = fmt.Sprintf(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":42,"date":1,`+
				`"chat":{"id":%d,"type":"private"},"from":{"id":%d,"is_bot":false,"first_name":"A"},"text":"hi"}}]}`,
				chat, owner)
		})
		time.Sleep(10 * time.Millisecond)

		return http.StatusOK, out
	})
	a := newAPI(t, f)
	h := &blockingHandler{started: make(chan context.Context, 1), release: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		a.Run(ctx, h)
		close(done)
	}()

	var hctx context.Context
	select {
	case hctx = <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never called")
	}
	cancel()

	select {
	case <-done:
		t.Fatal("Run returned while a handler was still running")
	case <-time.After(200 * time.Millisecond):
	}
	if hctx.Err() != nil {
		t.Fatalf("handler context cancelled by shutdown: %v", hctx.Err())
	}
	if _, ok := hctx.Deadline(); !ok {
		t.Fatal("handler context must be bounded by a timeout")
	}

	close(h.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the handler finished")
	}
}

func TestRejectedErrorsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp func(map[string]any) (int, string)
		want bool
	}{
		{"forbidden", apiError(http.StatusForbidden, "Forbidden: bot was blocked by the user"), true},
		{"chat not found", apiError(http.StatusBadRequest, "Bad Request: chat not found"), true},
		{"server error", apiError(http.StatusInternalServerError, "Internal Server Error"), false},
		{"flood", apiError(http.StatusTooManyRequests, "Too Many Requests: retry after 1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.on("sendMessage", tc.resp)
			f.on("editMessageText", tc.resp)
			f.on("getFile", tc.resp)
			a := newAPI(t, f)
			_, serr := a.Send(context.Background(), chat, 0, "x")
			_, eerr := a.Edit(context.Background(), chat, 77, "x")
			_, derr := a.Download(context.Background(), "id")
			for op, err := range map[string]error{"send": serr, "edit": eerr, "download": derr} {
				if err == nil || errors.Is(err, pipeline.ErrRejected) != tc.want {
					t.Errorf("%s: err %v, rejected want %v", op, err, tc.want)
				}
			}
		})
	}
}
