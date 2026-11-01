package capture

import (
	"testing"
	"time"
)

func TestJoinBatch(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	quiet := 3 * time.Second
	// open builds an unpicked batch due d from now, holding files at
	// positions 0, 4 and 2, text "first" and album "alb1".
	open := func(d time.Duration, mod func(*Capture)) *Capture {
		c := &Capture{
			Kind: KindFiles, Step: StepCollect, AlbumID: "alb1", Text: "first",
			NextAttemptAt: now.Add(d),
			Files:         []File{{Position: 0}, {Position: 4}, {Position: 2}},
		}
		if mod != nil {
			mod(c)
		}

		return c
	}

	tests := []struct {
		name      string
		open      *Capture
		item      BatchItem
		wantJoin  bool
		wantText  string
		wantPos   int
		wantMoved bool
	}{
		{"no open batch", nil, BatchItem{Caption: "x"}, false, "x", 0, true},
		{"still collecting", open(time.Second, nil), BatchItem{Caption: "b"}, true, "first\n\nb", 5, true},
		{"deadline reached", open(0, nil), BatchItem{Caption: "b"}, false, "b", 0, true},
		{
			"late album member",
			open(-time.Second, nil),
			BatchItem{AlbumID: "alb1", Caption: "c"},
			true,
			"first\n\nc",
			5,
			true,
		},
		{"other album", open(-time.Second, nil), BatchItem{AlbumID: "alb2"}, false, "", 0, true},
		{
			"empty album ids never match",
			open(-time.Second, func(c *Capture) { c.AlbumID = "" }),
			BatchItem{},
			false, "", 0, true,
		},
		{"empty caption keeps text", open(time.Second, nil), BatchItem{}, true, "first", 5, true},
		{"blank caption keeps text", open(time.Second, nil), BatchItem{Caption: " \n\t"}, true, "first", 5, true},
		{"blank caption starts no text", nil, BatchItem{Caption: "  "}, false, "", 0, true},
		{
			"empty open text takes caption",
			open(time.Second, func(c *Capture) { c.Text = "" }),
			BatchItem{Caption: "c"},
			true, "c", 5, true,
		},
		{
			"open without files",
			open(time.Second, func(c *Capture) { c.Files = nil }),
			BatchItem{},
			true, "first", 0, true,
		},
		{"deadline already later", open(time.Hour, nil), BatchItem{}, true, "first", 5, false},
		{
			"already picked",
			open(time.Second, func(c *Capture) { c.Step = StepDownload }),
			BatchItem{Caption: "c"},
			false, "c", 0, true,
		},
		{
			"not a files capture",
			open(time.Second, func(c *Capture) { c.Kind = KindAudio }),
			BatchItem{},
			false, "", 0, true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := JoinBatch(tt.open, tt.item, now, quiet)
			if got.Join != tt.wantJoin || got.Text != tt.wantText || got.Position != tt.wantPos ||
				got.DeadlineMoved != tt.wantMoved || !got.NextAttemptAt.Equal(now.Add(quiet)) {
				t.Errorf("JoinBatch = %+v, want join=%v text=%q pos=%d moved=%v next=%v",
					got, tt.wantJoin, tt.wantText, tt.wantPos, tt.wantMoved, now.Add(quiet))
			}
		})
	}
}
