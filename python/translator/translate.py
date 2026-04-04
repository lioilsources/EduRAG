#!/usr/bin/env python3
"""
python/translator/translate.py
Překládá anglické dokumenty do češtiny před embeddingem.

Strategie:
  - OPUS-MT (Helsinki-NLP) — lokální, rychlý, bez API
  - NLLB-200 (Meta) — lepší kvalita, větší model
  - Dávkové zpracování s cache (přeložené dokumenty se nemusí překládat znovu)

Kdy překládat:
  Pokud chceš české odpovědi s česky formulovaným kontextem,
  přeložení dokumentů PŘED embeddingem zlepší relevanci vyhledávání.
  Alternativou je vícejazyčný embedding (embed.py) — jednodušší, bez překladu.

  Doporučení: Začni s embed.py (bez překladu). Překlad přidej jen pokud
  výsledky nejsou dostatečně relevantní pro české dotazy.
"""
import argparse
import json
import logging
import os
import sqlite3
import sys
from pathlib import Path
from typing import Iterator

from tqdm import tqdm
from transformers import MarianMTModel, MarianTokenizer, pipeline

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger(__name__)

# Dostupné překladové modely
MODELS = {
    # OPUS-MT — rychlý, lokální, ~300MB
    "opus-en-cs": "Helsinki-NLP/opus-mt-en-cs",
    # OPUS-MT přes němčinu (EN→DE→CS, lepší kvalita pro některé texty)
    "opus-en-de": "Helsinki-NLP/opus-mt-en-de",
    # NLLB-200 — nejlepší kvalita, větší model ~600MB
    # Použití: lang kódy jsou "eng_Latn" → "ces_Latn"
    "nllb-600m": "facebook/nllb-200-distilled-600M",
    "nllb-1.3b": "facebook/nllb-200-distilled-1.3B",
}

DEFAULT_MODEL = "opus-en-cs"
DEFAULT_BATCH_SIZE = 16
MAX_CHUNK_CHARS = 400  # OPUS-MT má limit ~512 tokenů


class Translator:
    """Wrapper pro překladové modely s cache a dávkovým zpracováním."""

    def __init__(self, model_key: str = DEFAULT_MODEL, device: str = "cpu"):
        self.model_key = model_key
        self.device = device
        self.model_name = MODELS.get(model_key, model_key)
        self._pipe = None

    def _load(self) -> None:
        if self._pipe is not None:
            return

        log.info(f"Načítám překladový model: {self.model_name}")

        if "nllb" in self.model_name:
            self._pipe = pipeline(
                "translation",
                model=self.model_name,
                device=self.device,
                src_lang="eng_Latn",
                tgt_lang="ces_Latn",
                max_length=512,
            )
        else:
            # MarianMT (OPUS-MT)
            tokenizer = MarianTokenizer.from_pretrained(self.model_name)
            model = MarianMTModel.from_pretrained(self.model_name)
            self._pipe = pipeline(
                "translation",
                model=model,
                tokenizer=tokenizer,
                device=self.device,
                max_length=512,
            )

        log.info("Model načten ✓")

    def translate_batch(self, texts: list[str]) -> list[str]:
        """Přeloží seznam textů. Vrátí seznam přeložených textů."""
        self._load()

        # Rozděl dlouhé texty na chunky
        chunks_per_text: list[list[str]] = []
        for text in texts:
            chunks_per_text.append(_split_text(text, MAX_CHUNK_CHARS))

        # Flat list pro dávkový překlad
        flat_chunks = [c for chunks in chunks_per_text for c in chunks]

        if not flat_chunks:
            return [""] * len(texts)

        results = self._pipe(flat_chunks, batch_size=8)
        translated_flat = [r["translation_text"] for r in results]

        # Rekonstruuj původní strukturu
        output = []
        idx = 0
        for chunks in chunks_per_text:
            translated_chunks = translated_flat[idx: idx + len(chunks)]
            output.append(" ".join(translated_chunks))
            idx += len(chunks)

        return output

    def translate(self, text: str) -> str:
        """Přeloží jeden text."""
        return self.translate_batch([text])[0]


def _split_text(text: str, max_chars: int) -> list[str]:
    """Rozdělí text na kousky podle hranic vět."""
    if len(text) <= max_chars:
        return [text]

    sentences = []
    current = ""
    for sentence in text.replace("! ", ".\n").replace("? ", ".\n").split("."):
        sentence = sentence.strip()
        if not sentence:
            continue
        sentence += "."

        if len(current) + len(sentence) > max_chars:
            if current:
                sentences.append(current.strip())
            current = sentence
        else:
            current += " " + sentence

    if current.strip():
        sentences.append(current.strip())

    # Pokud stále příliš dlouhé, tvrdě ořízni
    result = []
    for s in sentences:
        if len(s) > max_chars:
            for i in range(0, len(s), max_chars):
                result.append(s[i: i + max_chars])
        else:
            result.append(s)

    return result if result else [text[:max_chars]]


# ─── Cache ────────────────────────────────────────────────────────────────────


class TranslationCache:
    """SQLite cache pro přeložené texty — zabraňuje opakovanému překladu."""

    def __init__(self, path: Path):
        self.conn = sqlite3.connect(str(path))
        self.conn.execute("""
            CREATE TABLE IF NOT EXISTS cache (
                hash    TEXT PRIMARY KEY,
                lang_from TEXT NOT NULL,
                lang_to   TEXT NOT NULL,
                translated TEXT NOT NULL
            )
        """)
        self.conn.commit()

    def get(self, text_hash: str) -> str | None:
        row = self.conn.execute(
            "SELECT translated FROM cache WHERE hash = ?", (text_hash,)
        ).fetchone()
        return row[0] if row else None

    def set(self, text_hash: str, lang_from: str, lang_to: str, translated: str) -> None:
        self.conn.execute(
            "INSERT OR REPLACE INTO cache (hash, lang_from, lang_to, translated) VALUES (?, ?, ?, ?)",
            (text_hash, lang_from, lang_to, translated),
        )
        self.conn.commit()

    def close(self) -> None:
        self.conn.close()


# ─── Pipeline ─────────────────────────────────────────────────────────────────


def _doc_hash(text: str) -> str:
    import hashlib
    return hashlib.sha256(text.encode()).hexdigest()[:16]


def translate_jsonl(
    input_path: Path,
    output_path: Path,
    cache_path: Path,
    translator: Translator,
    batch_size: int = DEFAULT_BATCH_SIZE,
    source_lang: str = "en",
    target_lang: str = "cs",
    translate_fields: list[str] = None,
) -> int:
    """
    Přečte JSONL, přeloží anglické dokumenty do češtiny, zapíše nový JSONL.
    Dokumenty, které již jsou v cílovém jazyce, přeskočí.
    Vrátí počet přeložených dokumentů.
    """
    if translate_fields is None:
        translate_fields = ["text", "title"]

    cache = TranslationCache(cache_path)
    output_path.parent.mkdir(parents=True, exist_ok=True)

    total_translated = 0
    total_skipped = 0

    # Načti všechny dokumenty pro dávkové zpracování
    docs: list[dict] = []
    with open(input_path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                try:
                    docs.append(json.loads(line))
                except json.JSONDecodeError:
                    continue

    log.info(f"Celkem dokumentů: {len(docs)}")

    with open(output_path, "w", encoding="utf-8") as out:
        for i in tqdm(range(0, len(docs), batch_size), desc="Překlad"):
            batch = docs[i: i + batch_size]

            # Separuj dokumenty k překladu od těch co překlad nepotřebují
            to_translate: list[tuple[int, str, str]] = []  # (doc_idx, field, text)
            for j, doc in enumerate(batch):
                if doc.get("lang", "en") != source_lang:
                    # Přeskočit — není anglicky
                    continue
                for field in translate_fields:
                    val = doc.get(field, "")
                    if val:
                        h = _doc_hash(val)
                        if cache.get(h) is None:
                            to_translate.append((j, field, val))

            # Dávkový překlad
            if to_translate:
                texts = [t[2] for t in to_translate]
                translated = translator.translate_batch(texts)

                for (j, field, original), result in zip(to_translate, translated):
                    h = _doc_hash(original)
                    cache.set(h, source_lang, target_lang, result)
                    batch[j][field] = result
                    batch[j][f"{field}_original"] = original

                total_translated += len(to_translate)
            else:
                total_skipped += len(batch)

            # Aktualizuj lang na "cs"
            for doc in batch:
                if doc.get("lang") == source_lang:
                    doc["lang"] = target_lang
                    doc["translated"] = True

            # Zapis do výstupu
            for doc in batch:
                out.write(json.dumps(doc, ensure_ascii=False) + "\n")

    cache.close()
    log.info(f"Přeloženo: {total_translated} textů, přeskočeno: {total_skipped}")
    return total_translated


# ─── CLI ──────────────────────────────────────────────────────────────────────


def main():
    parser = argparse.ArgumentParser(
        description="Přeložit JSONL dokumenty EN→CS před embeddingem"
    )
    parser.add_argument("--input", required=True, type=Path, help="Vstupní JSONL")
    parser.add_argument("--output", required=True, type=Path, help="Výstupní JSONL (přeložený)")
    parser.add_argument(
        "--cache", type=Path, default=Path("./data/translation_cache.db"),
        help="Cache databáze pro překlady"
    )
    parser.add_argument(
        "--model", default=DEFAULT_MODEL,
        choices=list(MODELS.keys()),
        help=f"Překladový model (default: {DEFAULT_MODEL})"
    )
    parser.add_argument(
        "--batch-size", type=int, default=DEFAULT_BATCH_SIZE,
        help=f"Batch size (default: {DEFAULT_BATCH_SIZE})"
    )
    parser.add_argument(
        "--device", default="cpu", choices=["cpu", "cuda", "mps"],
        help="Výpočetní zařízení (default: cpu)"
    )
    parser.add_argument(
        "--source-lang", default="en",
        help="Jazyk zdrojových dokumentů (default: en)"
    )
    parser.add_argument(
        "--target-lang", default="cs",
        help="Cílový jazyk (default: cs)"
    )

    args = parser.parse_args()

    if not args.input.exists():
        log.error(f"Vstupní soubor nenalezen: {args.input}")
        sys.exit(1)

    translator = Translator(model_key=args.model, device=args.device)

    count = translate_jsonl(
        input_path=args.input,
        output_path=args.output,
        cache_path=args.cache,
        translator=translator,
        batch_size=args.batch_size,
        source_lang=args.source_lang,
        target_lang=args.target_lang,
    )

    log.info(f"Hotovo — přeloženo {count} textů → {args.output}")
    print(f"\nDalší krok:")
    print(f"  python python/embedder/embed.py --input {args.output} --db ./data/chromadb")


if __name__ == "__main__":
    main()
