# rag-edu — RAG pipeline pro vzdělávání 1. stupně ZŠ

## Architektura

```
┌─────────────────────────────────────────────────────────┐
│                        GO VRSTVA                        │
│                                                         │
│  cmd/downloader     cmd/processor      cmd/server       │
│  ─────────────      ─────────────      ──────────       │
│  • Usenet/NNTP      • Čištění textu    • HTTP API       │
│  • Wikipedia dump   • Deduplikace      • /query         │
│  • Paralelní DL     • JSONL export     • /ingest        │
│  • Storage (SQLite) • Jazykdetekce     • /status        │
│                                                         │
└───────────────────────┬─────────────────────────────────┘
                        │ JSONL soubory / HTTP
┌───────────────────────▼─────────────────────────────────┐
│                     PYTHON VRSTVA                       │
│                                                         │
│  embedder/           rag/                               │
│  ─────────           ────                               │
│  • multilingual-e5   • ChromaDB vector store            │
│  • Dávkové embedding • Ollama / local LLM               │
│  • OPUS-MT překlad   • LangChain RAG chain              │
│                      • Czech query → odpověď            │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

## Prerekvizity

### Go
```
go >= 1.22
```

### Python
```
python >= 3.12
pip install -r python/requirements.txt
```

### Lokální LLM
```bash
# Nainstaluj Ollama: https://ollama.com
ollama pull llama3.2        # nebo mistral, qwen2.5
ollama pull nomic-embed-text  # záložní embedding
```

## Quickstart

```bash
# 1. Stáhni Wikipedia Simple English dump
go run cmd/downloader/main.go --source wiki --lang simple --topics "science,history,geography,gardening,philosophy"

# 2. Stáhni Usenet skupiny
go run cmd/downloader/main.go --source nntp \
  --server news.example.com \
  --groups "sci.bio,sci.geo.meteorology,rec.gardens,humanities.philosophy,soc.history" \
  --max-articles 5000

# 3. Zpracuj a vyčisti data
go run cmd/processor/main.go --input ./data/raw --output ./data/processed

# 4. Vytvoř embeddingy a naplň vector store
python python/embedder/embed.py --input ./data/processed --db ./data/chromadb

# 5. Spusť RAG server
python python/rag/server.py --db ./data/chromadb --model llama3.2

# 6. Dotazuj se česky
curl -X POST http://localhost:8080/query \
  -H "Content-Type: application/json" \
  -d '{"query": "Jak funguje fotosyntéza?"}'
```

## Struktura dat

```
data/
├── raw/           # stažená surová data (.eml, .xml)
├── processed/     # vyčištěné JSONL dokumenty
└── chromadb/      # vector store
```

## Překlad (volitelné)

Pokud vícejazyčný embedding (výchozí) nestačí, lze anglická data přeložit do češtiny:

```bash
# OPUS-MT (rychlý, lokální, ~300MB)
python python/translator/translate.py \
  --input ./data/processed/documents.jsonl \
  --output ./data/processed/documents_cs.jsonl \
  --model opus-en-cs

# NLLB-200 (lepší kvalita, ~600MB)
python python/translator/translate.py \
  --input ./data/processed/documents.jsonl \
  --output ./data/processed/documents_cs.jsonl \
  --model nllb-600m

# Poté embeduj přeložená data
python python/embedder/embed.py \
  --input ./data/processed/documents_cs.jsonl \
  --db ./data/chromadb
```

Překlady se cachují v SQLite — přerušený překlad pokračuje od posledního dokumentu.

## Testy

```bash
# Go testy
go test ./internal/...

# Python testy
pytest python/tests/ -v
```

```json
{"id": "wiki_123", "source": "wikipedia", "lang": "en", "topic": "science", "title": "Photosynthesis", "text": "Photosynthesis is..."}
{"id": "nntp_456", "source": "usenet", "lang": "en", "group": "sci.bio", "subject": "Re: plant growth", "text": "Plants need..."}
```
