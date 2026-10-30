package capture

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassifyHTTP(t *testing.T) {
	tests := []struct {
		status int
		want   Class
	}{
		{200, ClassOK},
		{201, ClassOK},
		{204, ClassOK},
		{500, ClassTemporary},
		{502, ClassTemporary},
		{503, ClassTemporary},
		{408, ClassTemporary},
		{429, ClassTemporary},
		{401, ClassMisconfig},
		{403, ClassMisconfig},
		{404, ClassMisconfig},
		{400, ClassPermanent},
		{413, ClassPermanent},
		{415, ClassPermanent},
		{422, ClassPermanent},
		{409, ClassTemporary},
		{302, ClassTemporary},
		{0, ClassTemporary},
	}
	for _, tt := range tests {
		if got := ClassifyHTTP(tt.status); got.Class != tt.want {
			t.Errorf("ClassifyHTTP(%d) = %v, want %v", tt.status, got.Class, tt.want)
		}
	}
	if got := ClassifyHTTP(404); got.Reason != "http 404" {
		t.Errorf("reason = %q", got.Reason)
	}
}

func TestOutcomeHelpers(t *testing.T) {
	o := ClassifyHTTP(503).On(WaitingSTT)
	if o.WaitingOn != WaitingSTT || o.Class != ClassTemporary {
		t.Errorf("On: %+v", o)
	}
	n := NetworkError(errors.New("dial tcp: timeout"))
	if n.Class != ClassTemporary || n.Reason != "dial tcp: timeout" {
		t.Errorf("NetworkError: %+v", n)
	}
	if NetworkError(nil).Class != ClassOK {
		t.Error("nil error must be OK")
	}
	if OK().Class != ClassOK || Permanent("bad").Class != ClassPermanent || Temporary("x").Class != ClassTemporary ||
		Misconfig("y").Class != ClassMisconfig {
		t.Error("constructors")
	}
	for _, c := range []Class{ClassOK, ClassTemporary, ClassMisconfig, ClassPermanent, Class(99)} {
		if c.String() == "" {
			t.Error("empty class string")
		}
	}
}

func TestNetworkErrorOmitsURL(t *testing.T) {
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "http://user:secret@127.0.0.1:1/path?token=abc", http.NoBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected a transport error")
	}
	out := NetworkError(err)
	if out.Class != ClassTemporary {
		t.Fatalf("class = %v", out.Class)
	}
	for _, leak := range []string{"user", "secret", "127.0.0.1:1/", "token", "abc", "/path"} {
		if strings.Contains(out.Reason, leak) {
			t.Errorf("reason %q leaks %q", out.Reason, leak)
		}
	}
	if !strings.HasPrefix(out.Reason, "Get") {
		t.Errorf("reason %q should start with the url.Error Op", out.Reason)
	}
	if got := NetworkError(errors.New("boom")).Reason; got != "boom" {
		t.Errorf("plain error reason = %q", got)
	}
	if NetworkError(nil).Class != ClassOK {
		t.Error("nil must be OK")
	}
}
