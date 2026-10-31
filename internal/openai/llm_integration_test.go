//go:build integration

package openai

// Opt-in repeat test of the LLM step against a real OpenAI-compatible
// endpoint. The model is not deterministic, so one good answer proves little:
// every case runs many times through the service's own path (BuildRequest,
// Chat.Complete, Parse) and the test reports how often each failure shows up.
// It is compiled only with the `integration` build tag and skips unless
// VOICENOTES_IT_LLM_BASE_URL is set, for example:
//
//	VOICENOTES_IT_LLM_BASE_URL=https://llm.example.org/v1 \
//	VOICENOTES_IT_LLM_API_KEY=... VOICENOTES_IT_LLM_MODEL=voicenotes \
//	go test -tags integration -run LLMRepeat -v -timeout 30m ./internal/openai
//
// VOICENOTES_IT_LLM_RUNS sets the runs per case (default 30) and
// VOICENOTES_IT_LLM_TEMPERATURE an optional temperature. Extra cases, such as
// real transcripts that must stay out of the repository, come from the file
// named by VOICENOTES_IT_LLM_CASES: one "<lang>|<expect tasks: y/n>|<text>"
// per line. Prompts and answers are never printed.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/enrich"
)

type llmCase struct {
	name      string
	lang      string // what speech-to-text would report
	wantTasks bool
	text      string
}

// builtinLLMCases are invented notes covering the shapes seen in practice.
func builtinLLMCases() []llmCase {
	return []llmCase{
		{"ru tasks", "ru", true, "Напомни мне завтра позвонить в банк и отправить документы бухгалтеру до пятницы."},
		{"ru no tasks", "ru", false, "Сегодня был отличный день, гуляли в парке, погода прекрасная, дети довольны."},
		{
			"ru punctuated tasks",
			"ru",
			true,
			"Не забыть: в субботу едем на дачу. По дороге нужно заехать в строительный " +
				"магазин, купить краску и кисти. А в понедельник записаться к стоматологу.",
		},
		{"ru short", "ru", true, "Купить хлеб."},
		{
			"ru one task",
			"ru",
			true,
			"У нас появились отличные идеи насчёт сотрудничества. До пятницы надо написать Ивану и предложить созвон.",
		},
		{"ru mixed en", "ru", true, "Надо обсудить с командой deadline по release и написать follow-up в Slack."},
		{"en tasks", "en", true, "Remind me to renew the car insurance next week and call the plumber about the leak."},
		{"en no tasks", "en", false, "Just an idea: a small app that tracks how often I water the plants."},
		{"de tasks", "de", true, "Morgen muss ich die Steuererklärung abschicken und Milch kaufen."},
	}
}

// llmTally counts the outcomes of one case.
type llmTally struct {
	ok, callErr, invalid, untranslated, noTasks, noTags int
	// flagOnly counts untranslated answers whose text is nevertheless in the
	// target language: the model translated but said it did not.
	flagOnly int
	// foreignTasks counts answers with an action item not in the target.
	foreignTasks int
	reasons      map[string]int
}

func TestLLMRepeatIntegration(t *testing.T) {
	base := os.Getenv("VOICENOTES_IT_LLM_BASE_URL")
	if base == "" {
		t.Skip("VOICENOTES_IT_LLM_BASE_URL is not set")
	}
	runs := 30
	if v := os.Getenv("VOICENOTES_IT_LLM_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatal("VOICENOTES_IT_LLM_RUNS must be a positive integer")
		}
		runs = n
	}
	cfg := ChatConfig{
		BaseURL: base, APIKey: os.Getenv("VOICENOTES_IT_LLM_API_KEY"),
		Model: os.Getenv("VOICENOTES_IT_LLM_MODEL"), Version: "it",
	}
	if v := os.Getenv("VOICENOTES_IT_LLM_TEMPERATURE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Fatal("VOICENOTES_IT_LLM_TEMPERATURE must be a number")
		}
		cfg.Temperature = &f
	}
	chat := NewChat(cfg, &http.Client{Timeout: 60 * time.Second})
	opts := enrich.Options{
		Translate: true, Tidy: true, Tasks: true, Target: "en", MaxTags: 3, InboxTag: "inbox", TodoTag: "todo",
	}
	cases := append(builtinLLMCases(), extraLLMCases(t)...)

	failed := false
	for _, c := range cases {
		tl := runLLMCase(chat, opts, c, runs)
		t.Logf(
			"%-22s ok %2d/%d · call errors %d · invalid %d · untranslated %d (flag only %d) · foreign tasks %d · "+
				"no tasks %d · no tags %d %v",
			c.name,
			tl.ok,
			runs,
			tl.callErr,
			tl.invalid,
			tl.untranslated,
			tl.flagOnly,
			tl.foreignTasks,
			tl.noTasks,
			tl.noTags,
			tl.reasons,
		)
		if tl.ok != runs {
			failed = true
		}
	}
	if failed {
		t.Error("some answers would not give a full note; see the counts above")
	}
}

// runLLMCase sends one case runs times, four at a time.
func runLLMCase(chat *Chat, opts enrich.Options, c llmCase, runs int) llmTally {
	tl := llmTally{reasons: map[string]int{}}
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for range runs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			raw, out := chat.Complete(context.Background(), toMessages(enrich.BuildRequest(opts, c.text, c.lang)))
			mu.Lock()
			defer mu.Unlock()
			if out.Class != capture.ClassOK {
				tl.callErr++
				tl.reasons[out.Reason]++

				return
			}
			res, err := enrich.Parse(raw, c.text, c.lang, opts)
			switch {
			case errors.Is(err, enrich.ErrNotTranslated):
				tl.untranslated++
				// Parse again without the language check to look at the text.
				if kept, perr := enrich.Parse(raw, c.text, "", opts); perr == nil && mostlyASCII(kept.Text) {
					tl.flagOnly++
				}
			case err != nil:
				tl.invalid++
				tl.reasons[strings.TrimPrefix(err.Error(), enrich.ErrInvalidOutput.Error()+": ")]++
			case c.lang != opts.Target && !res.Translated:
				tl.untranslated++
			case slices.ContainsFunc(res.Tasks, func(task string) bool { return !mostlyASCII(task) }):
				tl.foreignTasks++
			case c.wantTasks && len(res.Tasks) == 0:
				tl.noTasks++
			case len(res.Tags) == 0:
				tl.noTags++
			default:
				tl.ok++
			}
		})
	}
	wg.Wait()

	return tl
}

// mostlyASCII reports whether nine in ten letters of s are ASCII, which for
// the "en" target means the text was translated.
func mostlyASCII(s string) bool {
	var letters, ascii int
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
			if r < unicode.MaxASCII {
				ascii++
			}
		}
	}

	return letters > 0 && ascii*10 >= letters*9
}

func toMessages(in []enrich.Message) []Message {
	out := make([]Message, len(in))
	for i, m := range in {
		out[i] = Message{Role: m.Role, Content: m.Content}
	}

	return out
}

func extraLLMCases(t *testing.T) []llmCase {
	t.Helper()
	path := os.Getenv("VOICENOTES_IT_LLM_CASES")
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open VOICENOTES_IT_LLM_CASES: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []llmCase
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("VOICENOTES_IT_LLM_CASES line %d: want <lang>|<y/n>|<text>", n)
		}
		out = append(out, llmCase{
			name: fmt.Sprintf("extra %d (%s)", n, parts[0]), lang: parts[0], wantTasks: parts[1] == "y", text: parts[2],
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read VOICENOTES_IT_LLM_CASES: %v", err)
	}

	return out
}
