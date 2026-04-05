// internal/wiki/downloader_test.go
package wiki

import (
	"strings"
	"testing"
)

func TestCleanWikitext_RemovesTemplates(t *testing.T) {
	input := `Before template {{infobox plant | name = Rose | family = Rosaceae}} after template.`
	result := CleanWikitext(input)

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
	result := CleanWikitext(input)

	if strings.Contains(result, "<ref>") || strings.Contains(result, "</ref>") {
		t.Error("HTML tagy by měly být odstraněny")
	}
	if !strings.Contains(result, "need sunlight") {
		t.Error("Obsah by měl zůstat")
	}
}

func TestCleanWikitext_NestedTemplates(t *testing.T) {
	input := `Start {{outer {{inner value}} more}} end text here.`
	result := CleanWikitext(input)

	if strings.Contains(result, "{{") {
		t.Error("Vnořené šablony by měly být odstraněny")
	}
	if !strings.Contains(result, "Start") || !strings.Contains(result, "end text here") {
		t.Error("Text mimo šablony by měl zůstat")
	}
}

func TestCleanWikitext_HeadingsConvertedToNewlines(t *testing.T) {
	input := "== History ==\nSome historical content here.\n=== Early period ===\nMore content."
	result := CleanWikitext(input)

	if strings.Contains(result, "==") {
		t.Error("Wiki heading markery by měly být odstraněny")
	}
	if !strings.Contains(result, "Some historical content") {
		t.Error("Obsah pod nadpisem by měl zůstat")
	}
}

