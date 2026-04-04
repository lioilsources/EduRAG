#!/usr/bin/env python3
"""
python/embedder/embed.py
Čte JSONL dokumenty z Go pipeline, vytváří vícejazyčné embeddingy
a ukládá do ChromaDB vector store.

Vícejazyčný model intfloat/multilingual-e5-large zvládá česky i anglicky —
dotaz v češtině najde anglické dokumenty díky sdílenému embeddingového prostoru.
"""
import argparse
import json
import logging
import sys
from pathlib import Path
from typing import Generator

import chromadb
from chromadb.config import Settings
from sentence_transformers import SentenceTransformer
from tqdm import tqdm

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger(__name__)

# Doporučené modely (od nejlepšího pro CZ/EN):
# 1. intfloat/multilingual-e5-large   — nejlepší kvalita, 560MB
# 2. intfloat/multilingual-e5-base    — kompromis, 280MB
# 3. paraphrase-multilingual-mpnet-base-v2 — záloha, 278MB
DEFAULT_EMBED_MODEL = "intfloat/multilingual-e5-large"
DEFAULT_BATCH_SIZE = 64
DEFAULT_COLLECTION = "edu_docs"


def read_jsonl(path: Path) -> Generator[dict, None, None]:
    """Streamově čte JSONL soubor — efektivní pro velké soubory."""
    with open(path, encoding="utf-8") as f:
        for line_no, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                yield json.loads(line)
            except json.JSONDecodeError as e:
                log.warning(f"Řádek {line_no}: JSON chyba: {e}")
                continue


def prepare_text_for_embedding(doc: dict, model_name: str) -> str:
    """
    Připraví text pro embedding.
    multilingual-e5 vyžaduje prefix 'passage: ' pro dokumenty.
    """
    title = doc.get("title", "")
    text = doc.get("text", "")

    combined = f"{title}\n{text}" if title else text

    # E5 modely potřebují prefix
    if "e5" in model_name.lower():
        return f"passage: {combined}"
    return combined


def embed_documents(
    input_path: Path,
    db_path: Path,
    model_name: str = DEFAULT_EMBED_MODEL,
    batch_size: int = DEFAULT_BATCH_SIZE,
    collection_name: str = DEFAULT_COLLECTION,
    reset: bool = False,
) -> int:
    """
    Hlavní funkce: načte JSONL, vytvoří embeddingy, uloží do ChromaDB.
    Vrátí počet zpracovaných dokumentů.
    """
    log.info(f"Načítám embedding model: {model_name}")
    model = SentenceTransformer(model_name)
    log.info(f"Model načten, embedding dim: {model.get_sentence_embedding_dimension()}")

    # ChromaDB — persistentní lokální store
    db_path.mkdir(parents=True, exist_ok=True)
    client = chromadb.PersistentClient(
        path=str(db_path),
        settings=Settings(anonymized_telemetry=False),
    )

    if reset:
        try:
            client.delete_collection(collection_name)
            log.info(f"Kolekce '{collection_name}' smazána")
        except Exception:
            pass

    collection = client.get_or_create_collection(
        name=collection_name,
        metadata={
            "hnsw:space": "cosine",  # Cosine similarity — lepší pro texty
            "description": "RAG edu documents CZ/EN",
        },
    )

    existing_count = collection.count()
    log.info(f"Existující dokumenty v kolekci: {existing_count}")

    # Načti existující IDs pro přeskakování duplikátů
    existing_ids: set[str] = set()
    if existing_count > 0:
        results = collection.get(include=[])
        existing_ids = set(results["ids"])

    # Zpracuj JSONL v batchích
    batch_docs: list[dict] = []
    total_saved = 0
    total_skipped = 0

    def flush_batch(batch: list[dict]) -> None:
        nonlocal total_saved, total_skipped

        if not batch:
            return

        # Filtruj duplikáty
        new_docs = [d for d in batch if d.get("id", "") not in existing_ids]
        if not new_docs:
            total_skipped += len(batch)
            return

        texts = [prepare_text_for_embedding(d, model_name) for d in new_docs]
        embeddings = model.encode(
            texts,
            batch_size=batch_size,
            show_progress_bar=False,
            normalize_embeddings=True,  # Normalizace pro cosine similarity
        )

        collection.add(
            ids=[d["id"] for d in new_docs],
            embeddings=embeddings.tolist(),
            documents=[d["text"] for d in new_docs],
            metadatas=[
                {
                    "source": d.get("source", ""),
                    "lang": d.get("lang", "en"),
                    "group": d.get("group", ""),
                    "title": d.get("title", ""),
                }
                for d in new_docs
            ],
        )

        for d in new_docs:
            existing_ids.add(d["id"])

        total_saved += len(new_docs)
        total_skipped += len(batch) - len(new_docs)

    # Počet řádků pro progress bar
    log.info(f"Zpracovávám: {input_path}")
    with tqdm(unit=" docs", desc="Embedding") as pbar:
        for doc in read_jsonl(input_path):
            if not doc.get("text"):
                continue

            batch_docs.append(doc)
            pbar.update(1)

            if len(batch_docs) >= batch_size:
                flush_batch(batch_docs)
                batch_docs.clear()
                pbar.set_postfix(saved=total_saved, skip=total_skipped)

        # Zbývající batch
        flush_batch(batch_docs)

    log.info(
        f"Hotovo — uloženo: {total_saved}, přeskočeno (duplikáty): {total_skipped}"
    )
    log.info(f"Celkem v kolekci: {collection.count()}")
    return total_saved


def main():
    parser = argparse.ArgumentParser(
        description="Vytvoří embeddingy z JSONL a uloží do ChromaDB"
    )
    parser.add_argument(
        "--input", required=True, type=Path,
        help="JSONL soubor z Go procesoru"
    )
    parser.add_argument(
        "--db", required=True, type=Path,
        help="ChromaDB adresář"
    )
    parser.add_argument(
        "--model", default=DEFAULT_EMBED_MODEL,
        help=f"Embedding model (default: {DEFAULT_EMBED_MODEL})"
    )
    parser.add_argument(
        "--batch-size", type=int, default=DEFAULT_BATCH_SIZE,
        help=f"Batch size (default: {DEFAULT_BATCH_SIZE})"
    )
    parser.add_argument(
        "--collection", default=DEFAULT_COLLECTION,
        help=f"ChromaDB collection (default: {DEFAULT_COLLECTION})"
    )
    parser.add_argument(
        "--reset", action="store_true",
        help="Smaž existující kolekci před importem"
    )

    args = parser.parse_args()

    if not args.input.exists():
        log.error(f"Soubor nenalezen: {args.input}")
        sys.exit(1)

    embed_documents(
        input_path=args.input,
        db_path=args.db,
        model_name=args.model,
        batch_size=args.batch_size,
        collection_name=args.collection,
        reset=args.reset,
    )


if __name__ == "__main__":
    main()
