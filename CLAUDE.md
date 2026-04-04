# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

EduRAG is a bilingual (Czech/English) Retrieval-Augmented Generation (RAG) system for primary school education (grades 1–3). The pipeline is split between Go (data ingestion) and Python (ML, embeddings, serving).

## Commands

All common tasks are defined in the `Makefile`.

### Setup
```bash
make setup                  # Install Python deps + download Ollama model
```

### Data Pipeline
```bash
make download-wiki          # Download Simple English Wikipedia dump
make download-wiki-cs       # Download Czech Wikipedia dump
make download-nntp          # Download Usenet (requires NNTP_SERVER, NNTP_USER, NNTP_PASS env vars)
make process                # Export SQLite → JSONL
make embed                  # Create embeddings and populate ChromaDB
make serve                  # Start RAG server on http://localhost:8080
make pipeline               # Run full end-to-end pipeline
```

### Optional Translation (EN→CS)
```bash
make translate              # OPUS-MT translation (~300MB model)
make translate-nllb         # NLLB-200 translation (better quality, ~600MB)
make embed-cs               # Embed translated Czech documents
```

### Testing
```bash
make test-go                # Go unit tests
make test-python            # Python pytest
make test-all               # All tests
make test                   # Quick API smoke test (server must be running)
make test-garden            # Example query: garden/gardening
make test-history           # Example query: history topic
```

### Docker
```bash
docker-compose up           # Run complete stack (Ollama + RAG server)
```

## Architecture

### Data Flow
```
Data Sources (Wikipedia XML, Usenet NNTP)
    ↓  [Go layer]
cmd/downloader/main.go      → internal/wiki, internal/nntp
    ↓
internal/storage/db.go      → data/rag_edu.db (SQLite, WAL, deduplication by SHA256)
    ↓
cmd/processor/main.go       → data/processed/documents.jsonl
    ↓  [Python layer]
python/translator/          → data/processed/documents_cs.jsonl  (optional)
python/embedder/embed.py    → data/chromadb/  (ChromaDB vector store)
python/rag/server.py        → FastAPI on :8080
```

### Go Layer (`internal/`)
- **`pipeline/processor.go`** — Text cleaning, chunking (1500 chars, 150 overlap), language detection
- **`storage/db.go`** — SQLite with WAL journaling, hash-based dedup, state tracking for resumable downloads, JSONL export
- **`wiki/downloader.go`** — Streams and parses bzip2 Wikipedia XML dumps
- **`nntp/client.go`** — TLS/plain NNTP client with resume capability

### Python Layer (`python/`)
- **`rag/server.py`** — FastAPI server; receives Czech queries, runs LangChain RAG chain, returns answer + sources. Embedding model: `intfloat/multilingual-e5-large`. LLM via Ollama (llama3.2/mistral/qwen2.5). Temperature 0.3 for factual responses.
- **`embedder/embed.py`** — Streams JSONL, batches (64/batch), creates multilingual embeddings, loads into ChromaDB. Handles E5 model prefix requirements and duplicate detection.
- **`translator/translate.py`** — OPUS-MT or NLLB-200 for EN→CS translation; uses SQLite cache to avoid re-translating, supports resume.
- **`tests/`** — pytest with `conftest.py` fixtures; covers translation, embeddings, and pipeline logic.

### API Endpoints
| Endpoint | Method | Description |
|----------|--------|-------------|
| `/query` | POST | Process query → answer + sources |
| `/status` | GET | Server stats |
| `/health` | GET | Health check |

### Document Schema (JSONL)
```json
{
  "id": "wiki_abc123",
  "source": "wikipedia",
  "lang": "en",
  "group": "encyclopedia",
  "title": "Photosynthesis",
  "text": "...",
  "created_at": "2025-04-03T12:00:00Z"
}
```

## Key Design Decisions
- **Go for data ops, Python for ML** — clean separation; Go handles streaming/IO-heavy work efficiently, Python owns all ML inference
- **Local-first** — no external APIs; all LLM inference via Ollama
- **Single multilingual embedding model** — `multilingual-e5-large` handles Czech and English without separate models
- **Streaming JSONL** — all inter-stage data transfer uses streaming JSONL for memory efficiency on large corpora
- **Two-level deduplication** — SHA256 at SQLite storage stage and again at ChromaDB ingestion
