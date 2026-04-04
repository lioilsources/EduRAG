"""
python/tests/test_pipeline.py
Unit testy pro Python část pipeline.
Spuštění: pytest python/tests/ -v
"""
import json
import sys
import tempfile
from pathlib import Path

import pytest

# Přidej root do path
sys.path.insert(0, str(Path(__file__).parent.parent.parent))

from python.translator.translate import (
    Translator,
    TranslationCache,
    _doc_hash,
    _split_text,
)


# ─── Testy pro _split_text ────────────────────────────────────────────────────


def test_split_text_short_stays_single():
    text = "Short text."
    result = _split_text(text, 500)
    assert result == [text]


def test_split_text_long_splits():
    text = "First sentence about plants. Second sentence about animals. " * 20
    result = _split_text(text, 100)
    assert len(result) > 1
    for chunk in result:
        assert len(chunk) <= 150  # Mírná tolerance


def test_split_text_no_empty_chunks():
    text = "Sentence one. Sentence two. Sentence three. " * 10
    result = _split_text(text, 80)
    for i, chunk in enumerate(result):
        assert chunk.strip() != "", f"Chunk {i} je prázdný"


def test_split_text_reassembly_contains_all_content():
    """Všechen obsah by měl být v chuncích — nic by nemělo být ztraceno."""
    original_words = {"plants", "animals", "ecology", "biology", "nature"}
    text = "Plants and animals are part of ecology. Biology studies nature and living organisms. " * 5
    chunks = _split_text(text, 100)
    combined = " ".join(chunks).lower()
    for word in original_words:
        assert word in combined, f"Slovo '{word}' chybí v chuncích"


# ─── Testy pro TranslationCache ──────────────────────────────────────────────


def test_cache_set_and_get():
    with tempfile.NamedTemporaryFile(suffix=".db", delete=False) as f:
        cache_path = Path(f.name)

    try:
        cache = TranslationCache(cache_path)
        h = _doc_hash("Hello world")
        cache.set(h, "en", "cs", "Ahoj světe")

        result = cache.get(h)
        assert result == "Ahoj světe"

        cache.close()
    finally:
        cache_path.unlink(missing_ok=True)


def test_cache_returns_none_for_missing():
    with tempfile.NamedTemporaryFile(suffix=".db", delete=False) as f:
        cache_path = Path(f.name)

    try:
        cache = TranslationCache(cache_path)
        result = cache.get("nonexistent_hash")
        assert result is None
        cache.close()
    finally:
        cache_path.unlink(missing_ok=True)


def test_cache_overwrite():
    with tempfile.NamedTemporaryFile(suffix=".db", delete=False) as f:
        cache_path = Path(f.name)

    try:
        cache = TranslationCache(cache_path)
        h = _doc_hash("Test text")
        cache.set(h, "en", "cs", "První překlad")
        cache.set(h, "en", "cs", "Druhý překlad")  # Přepsat

        result = cache.get(h)
        assert result == "Druhý překlad"
        cache.close()
    finally:
        cache_path.unlink(missing_ok=True)


# ─── Testy pro _doc_hash ─────────────────────────────────────────────────────


def test_doc_hash_consistency():
    text = "Photosynthesis is important for life on Earth."
    assert _doc_hash(text) == _doc_hash(text)


def test_doc_hash_different_texts():
    h1 = _doc_hash("Text one about plants.")
    h2 = _doc_hash("Text two about animals.")
    assert h1 != h2


def test_doc_hash_length():
    """Hash by měl mít přesně 16 znaků (8 bytů hex)."""
    h = _doc_hash("Any text here")
    assert len(h) == 16


# ─── Testy pro JSONL translate pipeline (mock translator) ────────────────────


class MockTranslator:
    """Falešný translator pro testy bez skutečného ML modelu."""

    def translate_batch(self, texts: list[str]) -> list[str]:
        return [f"[CZ] {t}" for t in texts]

    def translate(self, text: str) -> str:
        return f"[CZ] {text}"


def test_translate_jsonl_basic():
    """Test translate_jsonl s mock translaterem."""
    from python.translator.translate import translate_jsonl

    # Vytvoř dočasné soubory
    with tempfile.TemporaryDirectory() as tmpdir:
        tmpdir = Path(tmpdir)
        input_path = tmpdir / "input.jsonl"
        output_path = tmpdir / "output.jsonl"
        cache_path = tmpdir / "cache.db"

        # Vstupní data
        docs = [
            {
                "id": "test_001",
                "source": "wikipedia",
                "lang": "en",
                "title": "Photosynthesis",
                "text": "Plants convert sunlight into energy.",
            },
            {
                "id": "test_002",
                "source": "usenet",
                "lang": "cs",  # Již česky — přeskočit
                "title": "Test",
                "text": "Toto je česky.",
            },
        ]

        with open(input_path, "w", encoding="utf-8") as f:
            for doc in docs:
                f.write(json.dumps(doc, ensure_ascii=False) + "\n")

        # Nahraď translator mockem
        import unittest.mock as mock
        with mock.patch.object(MockTranslator, "__class__"):
            translator = MockTranslator()
            count = translate_jsonl(
                input_path=input_path,
                output_path=output_path,
                cache_path=cache_path,
                translator=translator,
            )

        assert output_path.exists()

        # Zkontroluj výstup
        with open(output_path, encoding="utf-8") as f:
            result_docs = [json.loads(line) for line in f if line.strip()]

        assert len(result_docs) == 2

        # Anglický dokument by měl být přeložen
        en_doc = next(d for d in result_docs if d["id"] == "test_001")
        assert en_doc["lang"] == "cs"
        assert en_doc.get("translated") is True

        # Český dokument by měl zůstat nezměněn
        cs_doc = next(d for d in result_docs if d["id"] == "test_002")
        assert cs_doc["lang"] == "cs"
        assert "translated" not in cs_doc or cs_doc.get("translated") is not True


# ─── Testy pro embed.py utility funkce ───────────────────────────────────────


def test_prepare_text_for_embedding_e5_prefix():
    """E5 modely potřebují 'passage: ' prefix."""
    from python.embedder.embed import prepare_text_for_embedding

    doc = {"title": "Photosynthesis", "text": "Plants convert sunlight."}
    result = prepare_text_for_embedding(doc, "intfloat/multilingual-e5-large")
    assert result.startswith("passage: ")
    assert "Photosynthesis" in result
    assert "Plants convert sunlight" in result


def test_prepare_text_for_embedding_no_prefix():
    """Ostatní modely nepotřebují prefix."""
    from python.embedder.embed import prepare_text_for_embedding

    doc = {"title": "Test", "text": "Some content here."}
    result = prepare_text_for_embedding(doc, "paraphrase-multilingual-mpnet-base-v2")
    assert not result.startswith("passage: ")
    assert "Some content" in result


def test_prepare_text_for_embedding_missing_title():
    """Dokument bez titulku by měl fungovat."""
    from python.embedder.embed import prepare_text_for_embedding

    doc = {"text": "Just some text without a title."}
    result = prepare_text_for_embedding(doc, "some-model")
    assert "Just some text" in result


def test_read_jsonl_valid():
    """Test streamového čtení JSONL."""
    from python.embedder.embed import read_jsonl

    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".jsonl", delete=False, encoding="utf-8"
    ) as f:
        f.write('{"id": "1", "text": "hello"}\n')
        f.write('{"id": "2", "text": "world"}\n')
        f.write("\n")  # Prázdný řádek — přeskočit
        path = Path(f.name)

    try:
        docs = list(read_jsonl(path))
        assert len(docs) == 2
        assert docs[0]["id"] == "1"
        assert docs[1]["text"] == "world"
    finally:
        path.unlink(missing_ok=True)


def test_read_jsonl_invalid_line_skipped():
    """Neplatné JSON řádky by měly být přeskočeny."""
    from python.embedder.embed import read_jsonl

    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".jsonl", delete=False, encoding="utf-8"
    ) as f:
        f.write('{"id": "1", "text": "valid"}\n')
        f.write("INVALID JSON LINE\n")
        f.write('{"id": "3", "text": "also valid"}\n')
        path = Path(f.name)

    try:
        docs = list(read_jsonl(path))
        assert len(docs) == 2  # Neplatný řádek přeskočen
    finally:
        path.unlink(missing_ok=True)
