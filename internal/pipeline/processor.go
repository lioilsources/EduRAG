// internal/pipeline/processor.go
// Čistí, filtruje a normalizuje text z různých zdrojů.
// Připravuje dokumenty pro embedding pipeline.
package pipeline

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProcessorConfig konfigurace procesoru.
type ProcessorConfig struct {
	MinLength      int     // Minimální délka textu v znacích (default: 150)
	MaxLength      int     // Maximální délka textu (0 = bez limitu)
	MaxQuoteRatio  float64 // Max podíl quoted textu v Usenet článku (default: 0.5)
	RemoveSignature bool   // Odstranit Usenet signaturu (default: true)
}

// DefaultConfig vrátí výchozí konfiguraci.
func DefaultConfig() ProcessorConfig {
	return ProcessorConfig{
		MinLength:       150,
		MaxLength:       8000, // ~2000 tokenů — dobré pro RAG chunky
		MaxQuoteRatio:   0.5,
		RemoveSignature: true,
	}
}

// Processor čistí texty.
type Processor struct {
	cfg ProcessorConfig
}

// NewProcessor vytvoří nový Processor.
func NewProcessor(cfg ProcessorConfig) *Processor {
	return &Processor{cfg: cfg}
}

// ProcessUsenet vyčistí Usenet článek.
// Vrátí cleaned text nebo "" pokud článek nevyhovuje kvalitě.
func (p *Processor) ProcessUsenet(body string) string {
	// 1. Odstranit signaturu
	if p.cfg.RemoveSignature {
		body = removeSignature(body)
	}

	// 2. Spočítat quoted řádky (začínají > nebo |)
	lines := strings.Split(body, "\n")
	quotedLines := 0
	var contentLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ">") || strings.HasPrefix(trimmed, "|") {
			quotedLines++
		} else if trimmed != "" {
			contentLines = append(contentLines, trimmed)
		}
	}

	// 3. Odmítnout pokud příliš mnoho citací
	totalLines := len(lines)
	if totalLines > 0 {
		quoteRatio := float64(quotedLines) / float64(totalLines)
		if quoteRatio > p.cfg.MaxQuoteRatio {
			return ""
		}
	}

	// 4. Sestavit vyčištěný text
	text := strings.Join(contentLines, "\n")
	text = normalizeWhitespace(text)

	return p.applyLengthFilter(text)
}

// ProcessWiki vyčistí Wikipedia text.
func (p *Processor) ProcessWiki(text string) string {
	text = normalizeWhitespace(text)
	return p.applyLengthFilter(text)
}

// ChunkText rozdělí dlouhý text na chunky s překryvem.
// Překryv zajistí že kontext není ztracen na hranicích chunků.
func ChunkText(text string, maxChunkSize, overlap int) []string {
	if len(text) <= maxChunkSize {
		return []string{text}
	}

	var chunks []string
	runes := []rune(text)
	total := len(runes)

	start := 0
	for start < total {
		end := start + maxChunkSize
		if end > total {
			end = total
		}

		// Najdi konec věty (., !, ?) pokud je blízko
		if end < total {
			searchFrom := end - 100
			if searchFrom < start {
				searchFrom = start
			}
			for i := end; i > searchFrom; i-- {
				if runes[i-1] == '.' || runes[i-1] == '!' || runes[i-1] == '?' {
					end = i
					break
				}
			}
		}

		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}

		// Dosáhli jsme konce — konec smyčky
		if end >= total {
			break
		}

		// Posun s překryvem
		start = end - overlap
		if start < 0 {
			start = 0
		}
		if start >= end {
			start = end
		}
	}

	return chunks
}

// DetectLanguage jednoduchá heuristika pro detekci jazyka.
// Pro produkci použít golang.org/x/text/language nebo CLD3.
func DetectLanguage(text string) string {
	text = strings.ToLower(text)

	// České specifické znaky
	czCount := 0
	for _, r := range text {
		if strings.ContainsRune("áčďéěíňóřšťúůýžÁČĎÉĚÍŇÓŘŠŤÚŮÝŽ", r) {
			czCount++
		}
	}

	totalRunes := utf8.RuneCountInString(text)
	if totalRunes > 0 && float64(czCount)/float64(totalRunes) > 0.02 {
		return "cs"
	}

	// Základní check pro němčinu (ä, ö, ü, ß)
	deCount := 0
	for _, r := range text {
		if strings.ContainsRune("äöüßÄÖÜ", r) {
			deCount++
		}
	}
	if totalRunes > 0 && float64(deCount)/float64(totalRunes) > 0.02 {
		return "de"
	}

	return "en"
}

// IsEducational kontroluje zda text obsahuje vzdělávací obsah.
// Filtruje čistě diskuzní/sporné příspěvky bez faktické hodnoty.
func IsEducational(text string) bool {
	lower := strings.ToLower(text)

	// Negativní signály — flamewars, spam, one-liners
	negativePatterns := []string{
		"you're wrong", "you are wrong", "idiot", "moron",
		"fuck", "shit", "unsubscribe", "click here",
		"buy now", "limited offer",
	}
	for _, pat := range negativePatterns {
		if strings.Contains(lower, pat) {
			return false
		}
	}

	// Pozitivní signály — faktický obsah
	positivePatterns := []string{
		"because", "therefore", "however", "although",
		"example", "such as", "including", "known as",
		"discovered", "developed", "called", "named",
		"century", "year", "million", "species",
	}
	score := 0
	for _, pat := range positivePatterns {
		if strings.Contains(lower, pat) {
			score++
		}
	}

	return score >= 2
}

// --- Pomocné funkce ---

func removeSignature(text string) string {
	// Standardní Usenet signatura oddělena "-- " na vlastním řádku
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t\r") == "--" {
			return strings.Join(lines[:i], "\n")
		}
	}
	return text
}

func normalizeWhitespace(text string) string {
	// Nahradit vícenásobné mezery a newlines
	var sb strings.Builder
	sb.Grow(len(text))

	prevSpace := false
	prevNewline := false
	newlineCount := 0

	for _, r := range text {
		switch {
		case r == '\n':
			newlineCount++
			prevSpace = false
			if newlineCount <= 2 {
				sb.WriteRune('\n')
				prevNewline = true
			}
		case unicode.IsSpace(r):
			if !prevSpace && !prevNewline {
				sb.WriteRune(' ')
			}
			prevSpace = true
		default:
			sb.WriteRune(r)
			prevSpace = false
			prevNewline = false
			newlineCount = 0
		}
	}

	return strings.TrimSpace(sb.String())
}

func (p *Processor) applyLengthFilter(text string) string {
	runes := []rune(text)
	length := len(runes)

	if length < p.cfg.MinLength {
		return ""
	}

	if p.cfg.MaxLength > 0 && length > p.cfg.MaxLength {
		// Ořízni na konci věty
		text = string(runes[:p.cfg.MaxLength])
		lastDot := strings.LastIndexAny(text, ".!?")
		if lastDot > p.cfg.MaxLength/2 {
			text = text[:lastDot+1]
		}
	}

	return text
}
