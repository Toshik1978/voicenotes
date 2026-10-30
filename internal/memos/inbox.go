package memos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/digest"
)

const (
	defaultInboxTag = "inbox"
	// inboxPageSize keeps a page of long notes (content up to 8 KiB each,
	// plus snippet and metadata) well under maxBody.
	inboxPageSize = 50
	// maxInboxPages bounds the paging loop against a server that keeps
	// handing out tokens (400 pages of 50: the same 20000 notes as before).
	maxInboxPages = 400
)

// ListInbox returns the owner's notes tagged with the inbox tag, oldest first,
// following pagination (research R8). The filter uses the user name from
// auth/me, never a constructed one. Errors carry the outcome reason only: no
// token, URL or note content.
func (c *Client) ListInbox(ctx context.Context) ([]digest.NoteSummary, error) {
	prefix, out := c.resolvePrefix(ctx)
	if out.Class != capture.ClassOK {
		return nil, outcomeError(out)
	}
	owner, out := c.Owner(ctx)
	if out.Class != capture.ClassOK {
		return nil, outcomeError(out)
	}
	tag := c.cfg.InboxTag
	if tag == "" {
		tag = defaultInboxTag
	}
	filter := quoteFilter(tag) + " in tags && creator == " + quoteFilter(owner)

	var notes []digest.NoteSummary
	token, seen := "", map[string]bool{}
	for range maxInboxPages {
		page, next, err := c.fetchInboxPage(ctx, prefix, filter, token)
		if err != nil {
			return nil, err
		}
		notes = append(notes, page...)
		if next == "" {
			return notes, nil
		}
		if seen[next] {
			return nil, errors.New("memos list inbox: repeated page token")
		}
		seen[next], token = true, next
	}

	return nil, errors.New("memos list inbox: too many pages")
}

func (c *Client) fetchInboxPage(
	ctx context.Context,
	prefix, filter, token string,
) ([]digest.NoteSummary, string, error) {
	q := url.Values{}
	q.Set("filter", filter)
	q.Set("orderBy", "create_time asc")
	q.Set("pageSize", strconv.Itoa(inboxPageSize))
	if token != "" {
		q.Set("pageToken", token)
	}
	status, raw, out := c.do(ctx, http.MethodGet, prefix+"/memos?"+q.Encode(), nil)
	if out.Class != capture.ClassOK {
		return nil, "", outcomeError(out)
	}
	if status < 200 || status >= 300 {
		return nil, "", outcomeError(c.classify(status))
	}

	return c.parseInboxPage(raw)
}

func (c *Client) parseInboxPage(raw []byte) ([]digest.NoteSummary, string, error) {
	var page struct {
		Memos []struct {
			Name       string `json:"name"`
			Content    string `json:"content"`
			CreateTime string `json:"createTime"`
			Property   struct {
				HasTaskList bool `json:"hasTaskList"`
			} `json:"property"`
		} `json:"memos"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", errors.New("memos list inbox: malformed response")
	}
	notes := make([]digest.NoteSummary, 0, len(page.Memos))
	for _, m := range page.Memos {
		// One bad note must not block the whole digest: a note without a
		// usable name cannot be linked and is skipped; an unreadable
		// createTime only costs its ordering (zero time sorts first).
		if !validName(m.Name) {
			continue
		}
		created, err := time.Parse(time.RFC3339, m.CreateTime)
		if err != nil {
			created = time.Time{}
		}
		notes = append(notes, digest.NoteSummary{
			Name: m.Name, URL: c.link(m.Name), Content: m.Content,
			CreateTime: created, HasTaskList: m.Property.HasTaskList,
		})
	}

	return notes, page.NextPageToken, nil
}

// quoteFilter renders s as a double-quoted CEL string literal.
func quoteFilter(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)

	return `"` + s + `"`
}

func outcomeError(o capture.Outcome) error {
	return fmt.Errorf("memos list inbox: %s (%s)", o.Reason, o.Class)
}
