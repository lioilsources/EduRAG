// internal/wiki/downloader_test.go
package wiki

import (
	"strings"
	"testing"
)

func TestCleanWikitext_RemovesTemplates(t *testing.T) {
	input := `Before template {{infobox plant | name = Rose | family = Rosaceae}} after template.`
	result := cleanWikitext(input)

	if strings.Contains(result, "{{") || strings.Contains(result, "}}") {
		t.Error("Výsledek by neměl obsahovat šablony {{ }}")
	}
	if !strings.Contains(result, "Before template") {
		t.Error("Text před šablonou by měl zůstat")
	}
	if !strings.Contains(result, "after template") {
		t.Error("Text za šablonou by měl zůstat")
	}
}

func TestCleanWikitext_RemovesHTMLTags(t *testing.T) {
	input := `Plants <ref>Smith, 2020</ref> need sunlight. <br/> They also need water.`
	result := cleanWikitext(input)

	if strings.Contains(result, "<ref>") || strings.Contains(result, "</ref>") {
		t.Error("HTML tagy by měly být odstraněny")
	}
	if !strings.Contains(result, "need sunlight") {
		t.Error("Obsah by měl zůstat")
	}
}

func TestCleanWikitext_NestedTemplates(t *testing.T) {
	input := `Start {{outer {{inner value}} more}} end text here.`
	result := cleanWikitext(input)

	if strings.Contains(result, "{{") {
		t.Error("Vnořené šablony by měly být odstraněny")
	}
	if !strings.Contains(result, "Start") || !strings.Contains(result, "end text here") {
		t.Error("Text mimo šablony by měl zůstat")
	}
}

func TestCleanWikitext_HeadingsConvertedToNewlines(t *testing.T) {
	input := "== History ==\nSome historical content here.\n=== Early period ===\nMore content."
	result := cleanWikitext(input)

	if strings.Contains(result, "==") {
		t.Error("Wiki heading markery by měly být odstraněny")
	}
	if !strings.Contains(result, "Some historical content") {
		t.Error("Obsah pod nadpisem by měl zůstat")
	}
}

func TestMatchesTopic_TitleMatch(t *testing.T) {
	cfg := DownloadConfig{Topics: DefaultTopics}
	d := NewDownloader(cfg)

	// Přímý match v titulku
	if !d.matchesTopic("Photosynthesis", "Some random text") {
		t.Error("'photosynthesis' by měla matchovat téma")
	}
	if !d.matchesTopic("History of Ancient Rome", "Some text") {
		t.Error("'history' by mělo matchovat téma")
	}
	if !d.matchesTopic("Rose Garden", "Some text") {
		t.Error("'garden' by mělo matchovat téma")
	}
}

func TestMatchesTopic_TextMatch(t *testing.T) {
	cfg := DownloadConfig{Topics: DefaultTopics}
	d := NewDownloader(cfg)

	// Match v textu
	text := "This article discusses the ecology of rainforests and their biodiversity."
	if !d.matchesTopic("Random Title", text) {
		t.Error("'ecology' v textu by mělo matchovat téma")
	}
}

func TestMatchesTopic_NoMatch(t *testing.T) {
	cfg := DownloadConfig{Topics: DefaultTopics}
	d := NewDownloader(cfg)

	if d.matchesTopic("Stock Market Analysis", "Trading strategies and financial derivatives.") {
		t.Error("Finanční témata by neměla matchovat vzdělávací témata")
	}
	if d.matchesTopic("Celebrity Gossip", "Latest news about famous people.") {
		t.Error("Celebrity gossip by neměl matchovat")
	}
}

func TestParseDate_Formats(t *testing.T) {
	cases := []string{
		"Mon, 15 Jan 2024 10:30:00 +0100",
		"15 Jan 2024 10:30:00 +0100",
		"Mon, 15 Jan 2024 10:30:00 GMT",
	}

	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			result, err := parseDate(tc)
			if err != nil {
				t.Errorf("parseDate(%q) = error: %v", tc, err)
			}
			if result.IsZero() {
				t.Errorf("parseDate(%q) vrátil nulový čas", tc)
			}
		})
	}
}

func TestParseDate_InvalidFormat(t *testing.T) {
	_, err := parseDate("not a date at all")
	if err == nil {
		t.Error("Neplatné datum by mělo vrátit chybu")
	}
}
