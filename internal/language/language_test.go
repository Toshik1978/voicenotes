package language

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{"ru", "ru", true},
		{"russian", "ru", true},
		{"Russian", "ru", true},
		{"  RU\n", "ru", true},
		{"EN", "en", true},
		{"english", "en", true},
		{"castilian", "es", true},
		{"valencian", "ca", true},
		{"flemish", "nl", true},
		{"haitian", "ht", true},
		{"haitian creole", "ht", true},
		{"letzeburgesch", "lb", true},
		{"moldavian", "ro", true},
		{"moldovan", "ro", true},
		{"panjabi", "pa", true},
		{"pushto", "ps", true},
		{"sinhalese", "si", true},
		{"burmese", "my", true},
		{"mandarin", "zh", true},
		{"jw", "jv", true},
		{"javanese", "jv", true},
		{"hawaiian", "", false},
		{"haw", "", false},
		{"yue", "", false},
		{"cantonese", "", false},
		{"klingon", "", false},
		{"xx", "", false},
		{"", "", false},
		{"   ", "", false},
	}
	for _, tt := range tests {
		got, ok := Normalize(tt.raw)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("Normalize(%q) = %q, %v; want %q, %v", tt.raw, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestTableIsISO6391(t *testing.T) {
	for name, code := range lookup().names {
		if len(code) != 2 {
			t.Errorf("name %q maps to non-ISO-639-1 code %q", name, code)
		}
	}
	if len(lookup().codes) < 95 {
		t.Errorf("table has %d codes, want the full Whisper list", len(lookup().codes))
	}
}

func TestPlan(t *testing.T) {
	tests := []struct {
		name    string
		sources []string
		want    string
		forced  bool
	}{
		{"nil", nil, "", false},
		{"empty", []string{}, "", false},
		{"one", []string{"ru"}, "ru", true},
		{"one mixed case", []string{" RU "}, "ru", true},
		{"several", []string{"ru", "en"}, "", false},
	}
	for _, tt := range tests {
		got, forced := Plan(tt.sources)
		if got != tt.want || forced != tt.forced {
			t.Errorf("%s: Plan(%v) = %q, %v; want %q, %v", tt.name, tt.sources, got, forced, tt.want, tt.forced)
		}
	}
}

func TestRecheck(t *testing.T) {
	tests := []struct {
		name     string
		sources  []string
		detected string
		want     string
		redo     bool
	}{
		{"empty list accepts", nil, "german", "", false},
		{"one source accepts", []string{"ru"}, "english", "", false},
		{"several, detected listed (name)", []string{"ru", "en"}, "english", "", false},
		{"several, detected listed (code)", []string{"ru", "en"}, "ru", "", false},
		{"several, listed in other case", []string{"ru", "en"}, " EN ", "", false},
		{"several, not listed", []string{"ru", "en"}, "german", "ru", true},
		{"several, not listed code", []string{"en", "ru"}, "de", "en", true},
		{"several, not ISO-639-1", []string{"ru", "en"}, "hawaiian", "ru", true},
		{"several, unknown", []string{"ru", "en"}, "gibberish", "ru", true},
		{"several, none reported", []string{"ru", "en"}, "", "", false},
		{"several, blank reported", []string{"ru", "en"}, "  ", "", false},
		{"several, uppercase first source", []string{"RU", "en"}, "de", "ru", true},
	}
	for _, tt := range tests {
		got, redo := Recheck(tt.sources, tt.detected)
		if got != tt.want || redo != tt.redo {
			t.Errorf(
				"%s: Recheck(%v, %q) = %q, %v; want %q, %v",
				tt.name,
				tt.sources,
				tt.detected,
				got,
				redo,
				tt.want,
				tt.redo,
			)
		}
	}
}
