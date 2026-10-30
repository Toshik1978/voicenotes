package memos

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/voicenotes/internal/digest"
)

var _ digest.NotesReader = (*Client)(nil)

func TestListInboxPagesAndMaps(t *testing.T) {
	f := newFake("/api/v1")
	var queries []url.Values
	f.list = func(w http.ResponseWriter, q url.Values) {
		queries = append(queries, q)
		switch q.Get("pageToken") {
		case "":
			fmt.Fprint(
				w,
				`{"memos":[{"name":"memos/a","content":"one #inbox","createTime":"2026-03-04T05:06:07Z","property":{"hasTaskList":true}}],"nextPageToken":"p2"}`,
			)
		case "p2":
			fmt.Fprint(w, `{"memos":[{"name":"memos/b","content":"two","createTime":"2026-03-05T05:06:07Z"}]}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}
	c := newClient(t, f, nil)

	got, err := c.ListInbox(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "memos/a" || got[1].Name != "memos/b" {
		t.Fatalf("got %+v", got)
	}
	if got[0].URL != "https://memos.example.org/memos/a" || got[0].Content != "one #inbox" || !got[0].HasTaskList {
		t.Errorf("first = %+v", got[0])
	}
	if !got[0].CreateTime.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) || got[1].HasTaskList {
		t.Errorf("mapping = %+v", got)
	}
	if len(queries) != 2 {
		t.Fatalf("requests = %d, want 2", len(queries))
	}
	q := queries[0]
	if want := `"inbox" in tags && creator == "users/7"`; q.Get("filter") != want {
		t.Errorf("filter = %q, want %q", q.Get("filter"), want)
	}
	if q.Get("orderBy") != "create_time asc" || q.Get("pageSize") != "50" {
		t.Errorf("query = %v", q)
	}
	if queries[1].Get("pageToken") != "p2" || queries[1].Get("filter") != q.Get("filter") {
		t.Errorf("second query = %v", queries[1])
	}
	if f.lastAuth != "Bearer "+fakeToken {
		t.Errorf("auth = %q", f.lastAuth)
	}
}

func TestListInboxCustomTagIsQuoted(t *testing.T) {
	f := newFake("/api")
	var filter string
	f.list = func(w http.ResponseWriter, q url.Values) {
		filter = q.Get("filter")
		fmt.Fprint(w, `{}`)
	}
	c := newClient(t, f, func(cfg *Config) { cfg.InboxTag = `we"ird\` })

	got, err := c.ListInbox(ctx())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	if want := `"we\"ird\\" in tags && creator == "users/7"`; filter != want {
		t.Errorf("filter = %q, want %q", filter, want)
	}
}

func TestListInboxErrors(t *testing.T) {
	tests := []struct {
		name string
		list func(http.ResponseWriter, url.Values)
	}{
		{"server error", func(w http.ResponseWriter, _ url.Values) { w.WriteHeader(http.StatusInternalServerError) }},
		{"malformed", func(w http.ResponseWriter, _ url.Values) { fmt.Fprint(w, `<html>`) }},
		{"token loop", func(w http.ResponseWriter, _ url.Values) { fmt.Fprint(w, `{"nextPageToken":"same"}`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake("/api/v1")
			f.list = tt.list
			c := newClient(t, f, nil)
			if _, err := c.ListInbox(ctx()); err == nil {
				t.Fatal("want error")
			} else if strings.Contains(err.Error(), fakeToken) {
				t.Errorf("error leaks token: %v", err)
			}
		})
	}
}

func TestListInboxProbeFailure(t *testing.T) {
	f := newFake("/api/v1")
	f.probeStatus = http.StatusUnauthorized
	c := newClient(t, f, nil)
	if _, err := c.ListInbox(ctx()); err == nil {
		t.Fatal("want error")
	}
}

func TestListInboxExplicitPrefixFetchesOwner(t *testing.T) {
	f := newFake("/api")
	f.list = func(w http.ResponseWriter, q url.Values) {
		if !strings.Contains(q.Get("filter"), `creator == "users/7"`) {
			t.Errorf("filter = %q", q.Get("filter"))
		}
		fmt.Fprint(w, `{}`)
	}
	c := newClient(t, f, func(cfg *Config) { cfg.APIPrefix = "/api" })
	if _, err := c.ListInbox(ctx()); err != nil {
		t.Fatal(err)
	}
}

func TestListInboxToleratesOneBadNote(t *testing.T) {
	f := newFake("/api/v1")
	f.list = func(w http.ResponseWriter, _ url.Values) {
		fmt.Fprint(w, `{"memos":[`+
			`{"name":"x","createTime":"2026-03-04T05:06:07Z"},`+
			`{"name":"memos/a","createTime":"nope"},`+
			`{"name":"memos/b","createTime":"2026-03-05T05:06:07Z"}]}`)
	}
	got, err := newClient(t, f, nil).ListInbox(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "memos/a" || !got[0].CreateTime.IsZero() || got[1].Name != "memos/b" {
		t.Fatalf("got %+v: want the unnamed note skipped and the bad time zeroed", got)
	}
}
