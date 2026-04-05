.PHONY: setup download-wiki download-nntp process embed serve test

-include .env
export

# ─── Setup ────────────────────────────────────────────────────────────────────

setup:
	@echo "==> Instalace Python závislostí"
	pip install -r python/requirements.txt
	@echo "==> Go závislosti"
	go mod tidy
	@echo "✓ Setup hotov"

# ─── Data pipeline ────────────────────────────────────────────────────────────

download-wiki:
	@echo "==> Stahování Simple English Wikipedia (všechny články → wiki_simple.db)"
	go run cmd/downloader/main.go \
		--source wiki \
		--lang simple \
		--data-dir ./data

download-wiki-cs:
	@echo "==> Stahování České Wikipedie (všechny články → wiki_cs.db)"
	go run cmd/downloader/main.go \
		--source wiki \
		--lang cs \
		--data-dir ./data

download-wiki-en:
	@echo "==> Stahování English Wikipedia (všechny články → wiki_en.db, POZOR: ~20GB dump)"
	go run cmd/downloader/main.go \
		--source wiki \
		--lang en \
		--data-dir ./data

download-nntp:
	@echo "==> Stahování Usenet (nastav NNTP_SERVER, NNTP_USER, NNTP_PASS)"
	go run cmd/downloader/main.go \
		--source nntp \
		--server $(NNTP_SERVER) \
		--port $(or $(NNTP_PORT),119) \
		--user $(NNTP_USER) \
		--pass $(NNTP_PASS) \
		--tls=false \
		--groups "sci.bio,sci.geo.meteorology,rec.gardens,humanities.philosophy,soc.history,alt.history,sci.chem" \
		--max-articles 3000 \
		--workers 4 \
		--data-dir ./data

# ─── Filozofie / Seneca ───────────────────────────────────────────────────────
# Nový flow: 1) download-wiki-cs (jednou, plný dump → wiki_cs.db)
#            2) filter-philosophy (libovolně-krát, SQL filter → rag_edu.db)

filter-philosophy:
	@echo "==> Filtrace filozofických článků: wiki_cs.db → rag_edu.db"
	go run cmd/wiki-filter/main.go \
		--wiki-db ./data/wiki_cs.db \
		--rag-db ./data/rag_edu.db \
		--lang cs \
		--topics "Stoicismus,Seneca,Filozofie,Filosofie,Marcus Aurelius,Epiktétos,Epictetus,Stoicism,Stoic,Antika,Etika,Ctnost"

download-philosophy-wiki: download-wiki-cs filter-philosophy

download-philosophy-nntp:
	@echo "==> Stahování Usenet (filozofické skupiny)"
	go run cmd/downloader/main.go \
		--source nntp \
		--server $(NNTP_SERVER) \
		--port $(or $(NNTP_PORT),119) \
		--user $(NNTP_USER) \
		--pass $(NNTP_PASS) \
		--tls=false \
		--groups "alt.philosophy,alt.philosophy.stoicism,humanities.classics,rec.arts.books" \
		--max-articles 3000 \
		--workers 4 \
		--data-dir ./data

download-philosophy: download-philosophy-wiki download-philosophy-nntp

process:
	@echo "==> Export do JSONL"
	go run cmd/processor/main.go --data-dir ./data

embed:
	@echo "==> Vytváření embeddingů"
	python python/embedder/embed.py \
		--input ./data/processed/documents.jsonl \
		--db ./data/chromadb

serve:
	@echo "==> Spouštím RAG server na http://localhost:8080"
	python python/rag/server.py \
		--db ./data/chromadb \
		--model qwen3:32b \
		--port 8080

# ─── Celý pipeline najednou ───────────────────────────────────────────────────

pipeline: download-wiki process embed serve

# ─── Test ─────────────────────────────────────────────────────────────────────

test:
	@echo "==> Test dotazu"
	curl -s -X POST http://localhost:8080/query \
		-H "Content-Type: application/json" \
		-d '{"query": "Jak funguje fotosyntéza? Vysvětli jednoduše."}' \
		| python -m json.tool

test-garden:
	curl -s -X POST http://localhost:8080/query \
		-H "Content-Type: application/json" \
		-d '{"query": "Jak se pěstují rajčata na zahrádce?"}' \
		| python -m json.tool

test-history:
	curl -s -X POST http://localhost:8080/query \
		-H "Content-Type: application/json" \
		-d '{"query": "Kdo byli staří Egypťané a proč stavěli pyramidy?"}' \
		| python -m json.tool

translate:
	@echo "==> Překlad EN→CS (OPUS-MT)"
	python python/translator/translate.py \
		--input ./data/processed/documents.jsonl \
		--output ./data/processed/documents_cs.jsonl \
		--model opus-en-cs

translate-nllb:
	@echo "==> Překlad EN→CS (NLLB-200, lepší kvalita)"
	python python/translator/translate.py \
		--input ./data/processed/documents.jsonl \
		--output ./data/processed/documents_cs.jsonl \
		--model nllb-600m

embed-cs:
	@echo "==> Embedding přeložených dokumentů"
	python python/embedder/embed.py \
		--input ./data/processed/documents_cs.jsonl \
		--db ./data/chromadb

# ─── Testy ────────────────────────────────────────────────────────────────────

test-go:
	@echo "==> Go unit testy"
	go test ./internal/... -v

test-python:
	@echo "==> Python unit testy"
	pytest python/tests/ -v

test-all: test-go test-python
	@echo "✓ Všechny testy prošly"

status:
	curl -s http://localhost:8080/status | python -m json.tool

