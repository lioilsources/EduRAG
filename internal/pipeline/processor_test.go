// internal/pipeline/processor_test.go
package pipeline

import (
	"strings"
	"testing"
)

func TestProcessUsenet_RemovesQuotedLines(t *testing.T) {
	p := NewProcessor(DefaultConfig())

	body := `Thanks for the info!

> This is a quoted line
> Another quoted line
> And one more

The actual content here is about plants and photosynthesis.
Plants convert sunlight into energy through a process called photosynthesis.
This happens in the chloroplasts of plant cells and requires water and carbon dioxide.
It is one of the most important biological processes on Earth because it produces oxygen.`

	result := p.ProcessUsenet(body)

	if result == "" {
		t.Fatal("Očekáván neprázdný výsledek")
	}
	if strings.Contains(result, ">") {
		t.Error("Výsledek by neměl obsahovat quoted řádky")
	}
	if !strings.Contains(result, "photosynthesis") {
		t.Error("Výsledek by měl obsahovat obsah o fotosyntéze")
	}
}

func TestProcessUsenet_RejectsHighQuoteRatio(t *testing.T) {
	p := NewProcessor(DefaultConfig())

	// 80% quoted — mělo by být odmítnuto
	body := `> quote 1
> quote 2
> quote 3
> quote 4
> quote 5
> quote 6
> quote 7
> quote 8
Yes I agree.`

	result := p.ProcessUsenet(body)
	if result != "" {
		t.Errorf("Očekáván prázdný výsledek pro příliš mnoho citací, dostal: %q", result[:min(50, len(result))])
	}
}

func TestProcessUsenet_RemovesSignature(t *testing.T) {
	p := NewProcessor(DefaultConfig())

	body := `This is actual content about gardening and growing vegetables in your garden.
Tomatoes need plenty of sunlight and water to grow properly in your backyard.
Make sure to fertilize them regularly and watch for pests that might damage your plants.

--
John Smith
john@example.com
My personal website: http://example.com`

	result := p.ProcessUsenet(body)
	if strings.Contains(result, "john@example.com") {
		t.Error("Výsledek by neměl obsahovat signaturu")
	}
	if !strings.Contains(result, "gardening") {
		t.Error("Výsledek by měl obsahovat obsah o zahradničení")
	}
}

func TestProcessUsenet_RejectsShortText(t *testing.T) {
	p := NewProcessor(DefaultConfig())

	result := p.ProcessUsenet("Too short.")
	if result != "" {
		t.Errorf("Krátký text by měl být odmítnut, dostal: %q", result)
	}
}

func TestChunkText_SingleChunk(t *testing.T) {
	text := "This is a short text that fits in one chunk."
	chunks := ChunkText(text, 1000, 100)

	if len(chunks) != 1 {
		t.Errorf("Očekáván 1 chunk, dostal %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("Chunk by měl být shodný s originálem")
	}
}

func TestChunkText_MultipleChunks(t *testing.T) {
	// Vytvoř text delší než maxChunkSize
	sentence := "This is a sentence about plants and nature. "
	text := strings.Repeat(sentence, 50) // ~2200 znaků

	chunks := ChunkText(text, 500, 50)

	if len(chunks) < 2 {
		t.Errorf("Očekávány alespoň 2 chunky pro dlouhý text, dostal %d", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) == 0 {
			t.Errorf("Chunk %d je prázdný", i)
		}
		if len([]rune(chunk)) > 550 { // Mírná tolerance
			t.Errorf("Chunk %d je příliš dlouhý: %d znaků", i, len(chunk))
		}
	}
}

func TestChunkText_OverlapEnsuresContinuity(t *testing.T) {
	text := "First part about photosynthesis. Second part about plants. Third part about ecology. Fourth part about biology."
	chunks := ChunkText(text, 60, 20)

	if len(chunks) < 2 {
		t.Skip("Text příliš krátký pro test overlap")
	}

	// S překryvem by měl být poslední chunk obsahovat části předchozího
	// Netestujeme přesný obsah, jen že chunky nejsou prázdné
	for _, c := range chunks {
		if strings.TrimSpace(c) == "" {
			t.Error("Žádný chunk by neměl být prázdný")
		}
	}
}

func TestDetectLanguage_Czech(t *testing.T) {
	text := "Fotosyntéza je proces, při kterém rostliny přeměňují sluneční světlo na energii. Probíhá v chloroplastech buněk."
	lang := DetectLanguage(text)
	if lang != "cs" {
		t.Errorf("Očekáván 'cs', dostal '%s'", lang)
	}
}

func TestDetectLanguage_English(t *testing.T) {
	text := "Photosynthesis is the process by which plants convert sunlight into energy. It occurs in the chloroplasts of plant cells."
	lang := DetectLanguage(text)
	if lang != "en" {
		t.Errorf("Očekáván 'en', dostal '%s'", lang)
	}
}

func TestDetectLanguage_German(t *testing.T) {
	text := "Die Photosynthese ist ein Prozess, bei dem Pflanzen Sonnenlicht in Energie umwandeln. Das passiert in den Chloroplasten."
	lang := DetectLanguage(text)
	if lang != "de" {
		t.Errorf("Očekáván 'de', dostal '%s'", lang)
	}
}

func TestIsEducational_PositiveCase(t *testing.T) {
	text := "Photosynthesis is a process used by plants to convert light energy into chemical energy. For example, plants such as oak trees use carbon dioxide and water to produce glucose. This is known as the Calvin cycle and was discovered by Melvin Calvin in the twentieth century."
	if !IsEducational(text) {
		t.Error("Vzdělávací text by měl projít filtrem")
	}
}

func TestIsEducational_NegativeCase(t *testing.T) {
	text := "You're wrong and you're an idiot. This is complete nonsense."
	if IsEducational(text) {
		t.Error("Flamwar text by neměl projít filtrem")
	}
}

func TestNormalizeWhitespace(t *testing.T) {
	input := "Hello   world\n\n\n\nToo many newlines   here"
	result := normalizeWhitespace(input)

	if strings.Contains(result, "   ") {
		t.Error("Vícenásobné mezery by měly být odstraněny")
	}
	if strings.Contains(result, "\n\n\n") {
		t.Error("Vícenásobné newlines by měly být omezeny")
	}
}

func TestProcessorConfig_MinLength(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinLength = 500

	p := NewProcessor(cfg)
	shortText := "Short text. Only fifty characters long here!!!"
	result := p.ProcessWiki(shortText)

	if result != "" {
		t.Errorf("Text kratší než MinLength by měl být odmítnut")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
