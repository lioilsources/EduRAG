FROM python:3.12-slim

WORKDIR /app

COPY python/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY python/ ./python/

# Pre-download embedding modelu při buildu
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('intfloat/multilingual-e5-large')"

EXPOSE 8080

CMD ["python", "python/rag/server.py", "--db", "/app/data/chromadb"]
