#!/usr/bin/env python3
"""
python/rag/server.py
RAG server — přijímá české dotazy, hledá v ChromaDB, odpovídá přes lokální LLM.

Architektura:
  1. Dotaz v češtině → embedding (multilingual-e5)
  2. ChromaDB similarity search (funguje CZ→EN díky vícejazyčnému modelu)
  3. Sestavení promptu s kontextem
  4. Odpověď přes Ollama (lokální LLM)
  5. Odpověď v češtině (model odpovídá jazykem dotazu)
"""
import argparse
import json
import logging
import sys
from pathlib import Path

import chromadb
import uvicorn
from chromadb.config import Settings
from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from langchain.chains import RetrievalQA
from langchain.prompts import PromptTemplate
from langchain_chroma import Chroma
from langchain_community.llms import Ollama
from langchain_huggingface import HuggingFaceEmbeddings
from pydantic import BaseModel

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger(__name__)

# ─── Konfigurace ──────────────────────────────────────────────────────────────

DEFAULT_EMBED_MODEL = "intfloat/multilingual-e5-large"
DEFAULT_LLM_MODEL   = "qwen3:32b"   # nebo "mistral", "qwen2.5:7b"
DEFAULT_COLLECTION  = "edu_docs"
DEFAULT_TOP_K       = 5            # Počet dokumentů pro kontext

# Prompt navržený pro odpovědi v češtině s pedagogickým stylem
CZECH_EDU_PROMPT = PromptTemplate(
    input_variables=["context", "question"],
    template="""Jsi laskavý a trpělivý učitel pro žáky 1. stupně základní školy (6-11 let).
Odpovídáš vždy v češtině, srozumitelně a přátelsky.
Používáš jednoduché věty a konkrétní příklady.

Informace z učebnic a encyklopedií:
{context}

Otázka žáka: {question}

Tvoje odpověď (česky, srozumitelně pro dítě 6-11 let):""",
)

# ─── Modely ───────────────────────────────────────────────────────────────────


class QueryRequest(BaseModel):
    query: str
    top_k: int = DEFAULT_TOP_K
    lang_hint: str = "cs"  # nápověda pro prompt ("cs" = odpovídej česky)


class QueryResponse(BaseModel):
    answer: str
    sources: list[dict]
    query: str


class StatusResponse(BaseModel):
    status: str
    document_count: int
    embed_model: str
    llm_model: str
    collection: str


# ─── RAG Server ───────────────────────────────────────────────────────────────


class RAGServer:
    def __init__(
        self,
        db_path: Path,
        llm_model: str,
        embed_model: str,
        collection: str,
        ollama_url: str = "http://localhost:11434",
    ):
        self.collection_name = collection
        self.embed_model_name = embed_model
        self.llm_model_name = llm_model

        log.info(f"Načítám embedding model: {embed_model}")

        # Encode prefix pro query (E5 modely potřebují "query: " prefix pro dotazy)
        encode_kwargs = {"normalize_embeddings": True}
        model_kwargs = {"device": "cpu"}  # Změň na "cuda" pokud máš GPU

        self.embeddings = HuggingFaceEmbeddings(
            model_name=embed_model,
            model_kwargs=model_kwargs,
            encode_kwargs=encode_kwargs,
            # Pro E5 modely přidat query prefix
            query_instruction="query: " if "e5" in embed_model.lower() else "",
        )

        log.info(f"Připojuji ChromaDB: {db_path}")
        chroma_client = chromadb.PersistentClient(
            path=str(db_path),
            settings=Settings(anonymized_telemetry=False),
        )

        self.vectorstore = Chroma(
            client=chroma_client,
            collection_name=collection,
            embedding_function=self.embeddings,
        )

        self.doc_count = self.vectorstore._collection.count()
        log.info(f"ChromaDB připojena, dokumentů: {self.doc_count}")

        log.info(f"Připojuji Ollama LLM: {llm_model} @ {ollama_url}")
        self.llm = Ollama(
            model=llm_model,
            base_url=ollama_url,
            temperature=0.3,       # Nízká teplota = faktičtější odpovědi
            num_predict=512,       # Max délka odpovědi
        )

        # Retrieval QA chain
        self.retriever = self.vectorstore.as_retriever(
            search_type="similarity",
            search_kwargs={"k": DEFAULT_TOP_K},
        )

        self.qa_chain = RetrievalQA.from_chain_type(
            llm=self.llm,
            chain_type="stuff",  # Všechny dokumenty do jednoho promptu
            retriever=self.retriever,
            return_source_documents=True,
            chain_type_kwargs={"prompt": CZECH_EDU_PROMPT},
        )

        log.info("RAG server připraven ✓")

    def query(self, question: str, top_k: int = DEFAULT_TOP_K) -> QueryResponse:
        """Zpracuje dotaz a vrátí odpověď s citacemi zdrojů."""
        # Dynamický retriever s vlastním top_k
        retriever = self.vectorstore.as_retriever(
            search_type="similarity",
            search_kwargs={"k": top_k},
        )

        chain = RetrievalQA.from_chain_type(
            llm=self.llm,
            chain_type="stuff",
            retriever=retriever,
            return_source_documents=True,
            chain_type_kwargs={"prompt": CZECH_EDU_PROMPT},
        )

        result = chain.invoke({"query": question})

        # Extrahuj zdroje
        sources = []
        for doc in result.get("source_documents", []):
            meta = doc.metadata or {}
            sources.append({
                "title": meta.get("title", "Neznámý zdroj"),
                "source": meta.get("source", ""),
                "lang": meta.get("lang", "en"),
                "group": meta.get("group", ""),
                "excerpt": doc.page_content[:200] + "..." if len(doc.page_content) > 200 else doc.page_content,
            })

        return QueryResponse(
            answer=result["result"],
            sources=sources,
            query=question,
        )

    def status(self) -> StatusResponse:
        return StatusResponse(
            status="ok",
            document_count=self.vectorstore._collection.count(),
            embed_model=self.embed_model_name,
            llm_model=self.llm_model_name,
            collection=self.collection_name,
        )


# ─── FastAPI app ──────────────────────────────────────────────────────────────


def create_app(server: RAGServer) -> FastAPI:
    app = FastAPI(
        title="RAG Edu API",
        description="RAG server pro vzdělávací obsah 1. stupně ZŠ",
        version="1.0.0",
    )

    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_methods=["*"],
        allow_headers=["*"],
    )

    @app.get("/status", response_model=StatusResponse)
    def status():
        """Stav serveru a statistiky."""
        return server.status()

    @app.post("/query", response_model=QueryResponse)
    def query(req: QueryRequest):
        """
        Zpracuje dotaz v češtině a vrátí odpověď s citacemi.

        Příklad:
        ```json
        {"query": "Jak funguje fotosyntéza?"}
        ```
        """
        if not req.query.strip():
            raise HTTPException(status_code=400, detail="Prázdný dotaz")

        try:
            return server.query(req.query, req.top_k)
        except Exception as e:
            log.error(f"Query error: {e}", exc_info=True)
            raise HTTPException(status_code=500, detail=str(e))

    @app.get("/health")
    def health():
        return {"ok": True}

    return app


# ─── Main ─────────────────────────────────────────────────────────────────────


def main():
    parser = argparse.ArgumentParser(description="RAG Edu Server")
    parser.add_argument("--db", required=True, type=Path, help="ChromaDB adresář")
    parser.add_argument("--model", default=DEFAULT_LLM_MODEL, help="Ollama model")
    parser.add_argument("--embed-model", default=DEFAULT_EMBED_MODEL, help="Embedding model")
    parser.add_argument("--collection", default=DEFAULT_COLLECTION, help="ChromaDB kolekce")
    parser.add_argument("--ollama-url", default="http://localhost:11434", help="Ollama URL")
    parser.add_argument("--host", default="0.0.0.0", help="Host")
    parser.add_argument("--port", type=int, default=8080, help="Port")
    args = parser.parse_args()

    if not args.db.exists():
        log.error(f"ChromaDB adresář nenalezen: {args.db}")
        log.error("Nejprve spusť: python python/embedder/embed.py")
        sys.exit(1)

    rag = RAGServer(
        db_path=args.db,
        llm_model=args.model,
        embed_model=args.embed_model,
        collection=args.collection,
        ollama_url=args.ollama_url,
    )

    app = create_app(rag)

    log.info(f"Server spuštěn: http://{args.host}:{args.port}")
    log.info(f"API docs: http://{args.host}:{args.port}/docs")
    uvicorn.run(app, host=args.host, port=args.port)


if __name__ == "__main__":
    main()
