"""Embedding and reranking adapters. A new provider is one small class plus one registry entry.

Embedder.embed(texts, role) returns an (n, dim) float32 array of L2-normalised vectors; role is
"document" for indexed chunks and "query" for search text. Reranker.rerank(query, docs) returns
one relevance score per doc, higher is better. Every adapter records its calls and tokens in
USAGE so the run can report spend.
"""

import json
import os
import threading
import time
import urllib.request
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor

import numpy as np

USAGE: dict[str, dict[str, float]] = defaultdict(lambda: defaultdict(float))
_usage_lock = threading.Lock()


def _count(provider: str, **kw: float) -> None:
    with _usage_lock:
        for k, v in kw.items():
            USAGE[provider][k] += v


def _normalise(a: np.ndarray) -> np.ndarray:
    a = np.asarray(a, dtype=np.float32)
    return a / np.linalg.norm(a, axis=1, keepdims=True).clip(min=1e-12)


def _retry(fn, attempts: int = 8):
    for i in range(attempts):
        try:
            return fn()
        except Exception as e:  # throttling and transient network errors
            msg = str(e)
            transient = any(
                s in msg
                for s in ("Throttl", "429", "500", "502", "503", "504", "timed out", "Too many", "RESOURCE_EXHAUSTED", "reset", "Try your request again")
            )
            if not transient or i == attempts - 1:
                raise
            time.sleep(min(60, 2**i))


def _post_json(url: str, body: dict, headers: dict) -> dict:
    req = urllib.request.Request(
        url, data=json.dumps(body).encode(), method="POST", headers={"Content-Type": "application/json", **headers}
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"POST {url.split('?')[0]} -> {e.code}: {e.read().decode()[:400]}") from e


# --------------------------------------------------------------------------------------------
# Bedrock


_bedrock = {}


def bedrock(region: str = "us-west-2"):
    if region not in _bedrock:
        import boto3
        from botocore.config import Config

        # The standard AWS chain picks the account (AWS_PROFILE or its default); the models must be enabled there.
        session = boto3.Session()
        _bedrock[region] = session.client(
            "bedrock-runtime", region_name=region, config=Config(retries={"max_attempts": 10, "mode": "adaptive"}, max_pool_connections=32)
        )
    return _bedrock[region]


def _invoke(model_id: str, body: dict) -> tuple[dict, int]:
    def call():
        return bedrock().invoke_model(modelId=model_id, body=json.dumps(body), contentType="application/json", accept="application/json")

    resp = _retry(call)
    tokens = int(resp["ResponseMetadata"]["HTTPHeaders"].get("x-amzn-bedrock-input-token-count", 0))
    return json.loads(resp["body"].read()), tokens


class Embedder:
    name: str
    dim: int
    max_chars: int  # inputs are cut to this many characters before sending (model context limit)
    batch: int = 64
    concurrency: int = 4

    def _embed_batch(self, texts: list[str], role: str) -> np.ndarray:
        raise NotImplementedError

    def embed(self, texts: list[str], role: str) -> np.ndarray:
        texts = [t[: self.max_chars] for t in texts]
        batches = [texts[i : i + self.batch] for i in range(0, len(texts), self.batch)]
        with ThreadPoolExecutor(self.concurrency) as ex:
            parts = list(ex.map(lambda b: self._embed_batch(b, role), batches))
        return _normalise(np.vstack(parts)) if parts else np.zeros((0, self.dim), np.float32)


class CohereEmbedV4(Embedder):
    """Cohere Embed v4 on Bedrock (US cross-region profile), asymmetric input types."""

    name, dim, batch, concurrency = "cohere-embed-v4", 1536, 96, 6
    max_chars = 30_000  # 8,192-token context on Cohere's model card
    model_id = "us.cohere.embed-v4:0"

    def _embed_batch(self, texts, role):
        body = {
            "texts": texts,
            "input_type": "search_document" if role == "document" else "search_query",
            "embedding_types": ["float"],
            "output_dimension": self.dim,
        }
        out, tokens = _invoke(self.model_id, body)
        _count("bedrock:" + self.model_id, calls=1, input_tokens=tokens)
        return np.array(out["embeddings"]["float"], np.float32)


class TitanEmbedV2(Embedder):
    """Amazon Titan Text Embeddings v2 on Bedrock: one text per call, symmetric (no input type)."""

    name, dim, batch, concurrency = "titan-embed-v2", 1024, 1, 16
    max_chars = 30_000  # 8,192-token limit
    model_id = "amazon.titan-embed-text-v2:0"

    def _embed_batch(self, texts, role):
        out, _ = _invoke(self.model_id, {"inputText": texts[0], "dimensions": self.dim, "normalize": True})
        _count("bedrock:" + self.model_id, calls=1, input_tokens=out["inputTextTokenCount"])
        return np.array([out["embedding"]], np.float32)


class GeminiEmbedding(Embedder):
    """Google gemini-embedding-001, RETRIEVAL_DOCUMENT / RETRIEVAL_QUERY task types, 3072 dims."""

    name, dim, batch, concurrency = "gemini-embedding-001", 3072, 100, 4
    max_chars = 7_000  # 2,048-token limit
    url = "https://generativelanguage.googleapis.com/v1beta/models/gemini-embedding-001:batchEmbedContents"

    def _embed_batch(self, texts, role):
        task = "RETRIEVAL_DOCUMENT" if role == "document" else "RETRIEVAL_QUERY"
        body = {
            "requests": [
                {"model": "models/gemini-embedding-001", "content": {"parts": [{"text": t}]}, "taskType": task}
                for t in texts
            ]
        }
        out = _retry(lambda: _post_json(self.url, body, {"x-goog-api-key": os.environ["GEMINI_API_KEY"]}))
        # The batch endpoint reports no token usage; estimate at 4 characters per token.
        _count("gemini:gemini-embedding-001", calls=1, input_tokens_estimated=sum(len(t) for t in texts) / 4)
        return np.array([e["values"] for e in out["embeddings"]], np.float32)


class OpenAIEmbedding3Large(Embedder):
    """OpenAI text-embedding-3-large, 3072 dims, symmetric."""

    name, dim, batch, concurrency = "openai-3-large", 3072, 128, 4
    max_chars = 24_000  # 8,191-token limit
    url = "https://api.openai.com/v1/embeddings"

    def _embed_batch(self, texts, role):
        body = {"model": "text-embedding-3-large", "input": texts}
        out = _retry(lambda: _post_json(self.url, body, {"Authorization": "Bearer " + os.environ["OPENAI_API_KEY"]}))
        _count("openai:text-embedding-3-large", calls=1, input_tokens=out["usage"]["total_tokens"])
        return np.array([d["embedding"] for d in sorted(out["data"], key=lambda d: d["index"])], np.float32)


class CohereAPIEmbedding(Embedder):
    """Cohere's own API (v2 embed), asymmetric input types; for models Bedrock does not carry yet."""

    batch, concurrency = 96, 4
    max_chars = 30_000  # 8,192-token context; the API also truncates the end by default
    url = "https://api.cohere.com/v2/embed"

    def __init__(self, model: str, dim: int):
        self.model, self.dim, self.name = model, dim, model

    def _embed_batch(self, texts, role):
        body = {
            "model": self.model,
            "texts": texts,
            "input_type": "search_document" if role == "document" else "search_query",
            "embedding_types": ["float"],
        }
        out = _retry(lambda: _post_json(self.url, body, {"Authorization": "Bearer " + os.environ["COHERE_API_KEY"]}))
        _count("cohere:" + self.model, calls=1, input_tokens=out["meta"]["billed_units"]["input_tokens"])
        return np.array(out["embeddings"]["float"], np.float32)


class VoyageEmbedding(Embedder):
    """Voyage AI embeddings (api.voyageai.com), document / query input types, default dimension."""

    batch, concurrency = 64, 4  # 64 chunks stays under the 120k-token batch limit of the large model
    max_chars = 100_000  # 32k-token context; the API truncates by default
    url = "https://api.voyageai.com/v1/embeddings"

    def __init__(self, model: str, dim: int = 1024):
        self.model, self.dim, self.name = model, dim, model

    def _embed_batch(self, texts, role):
        body = {"model": self.model, "input": texts, "input_type": role}
        out = _retry(lambda: _post_json(self.url, body, {"Authorization": "Bearer " + os.environ["VOYAGE_API_KEY"]}))
        _count("voyage:" + self.model, calls=1, input_tokens=out["usage"]["total_tokens"])
        return np.array([d["embedding"] for d in sorted(out["data"], key=lambda d: d["index"])], np.float32)


def _torch_threads():
    """Few threads per process: on a loaded machine, more OpenMP threads than free cores makes
    inference several times slower (measured: 4 threads 1.2 s/chunk, 8 threads 6.1 s/chunk)."""
    import torch

    torch.set_num_threads(int(os.environ.get("SEARCHBENCH_TORCH_THREADS", "4")))
    return torch


class QwenEmbedding(Embedder):
    """Qwen3-Embedding run locally on CPU in bfloat16 with sentence-transformers."""

    batch, concurrency = 100_000, 1  # sentence-transformers batches internally
    max_chars = 32_000
    instruction = "Given a search query, retrieve the issues and recorded decisions of an engineering issue tracker that answer it"

    def __init__(self, size: str = "0.6B", max_tokens: int = 8192):
        torch = _torch_threads()
        from sentence_transformers import SentenceTransformer

        self.name = f"qwen3-embedding-{size.lower()}"
        self.model = SentenceTransformer(
            f"Qwen/Qwen3-Embedding-{size}", device="cpu", model_kwargs={"dtype": torch.bfloat16}, processor_kwargs={"padding_side": "left"}
        )
        self.model.max_seq_length = max_tokens
        self.dim = self.model.get_sentence_embedding_dimension()

    def _embed_batch(self, texts, role):
        t0 = time.time()
        kw = {"prompt": f"Instruct: {self.instruction}\nQuery:"} if role == "query" else {}
        out = self.model.encode(texts, batch_size=16, convert_to_numpy=True, normalize_embeddings=True, **kw)
        _count("local:" + self.name, calls=1, texts=len(texts), cpu_seconds=time.time() - t0)
        return out.astype(np.float32)


# --------------------------------------------------------------------------------------------
# Rerankers


class Reranker:
    name: str
    max_doc_chars: int = 4_000
    max_query_chars: int = 4_000

    def rerank(self, query: str, docs: list[str]) -> list[float]:
        raise NotImplementedError


class _BedrockRerank(Reranker):
    model_id: str
    per_call = 100  # Bedrock bills rerank per request of up to 100 documents

    def rerank(self, query, docs):
        query = query[: self.max_query_chars]
        docs = [d[: self.max_doc_chars] for d in docs]
        scores = [0.0] * len(docs)
        for start in range(0, len(docs), self.per_call):
            part = docs[start : start + self.per_call]
            out, tokens = _invoke(self.model_id, self._body(query, part))
            # Bedrock splits a document longer than its window into chunks and bills per 100 chunks,
            # so record the characters sent as well as the requests, to price the run conservatively.
            chars = sum(len(query) + len(d) for d in part)
            _count("bedrock:" + self.model_id, calls=1, search_units=1, input_tokens=tokens, pair_chars=chars)
            for r in out["results"]:
                scores[start + r["index"]] = r["relevance_score"]
        return scores


class CohereRerank35(_BedrockRerank):
    name, model_id = "cohere-rerank-3.5", "cohere.rerank-v3-5:0"

    def _body(self, query, docs):
        return {"query": query, "documents": docs, "top_n": len(docs), "api_version": 2}


class AmazonRerankV1(_BedrockRerank):
    name, model_id = "amazon-rerank-v1", "amazon.rerank-v1:0"

    def _body(self, query, docs):
        return {"query": query, "documents": docs, "top_n": len(docs)}


class CohereAPIRerank(Reranker):
    """Cohere's own API (v2 rerank). Billed per search unit: one query with up to 100 documents."""

    url = "https://api.cohere.com/v2/rerank"

    def __init__(self, model: str):
        self.model, self.name = model, model

    def rerank(self, query, docs):
        body = {
            "model": self.model,
            "query": query[: self.max_query_chars],
            "documents": [d[: self.max_doc_chars] for d in docs],
            "top_n": len(docs),
        }
        out = _retry(lambda: _post_json(self.url, body, {"Authorization": "Bearer " + os.environ["COHERE_API_KEY"]}))
        _count("cohere:" + self.model, calls=1, search_units=out["meta"]["billed_units"]["search_units"])
        scores = [0.0] * len(docs)
        for r in out["results"]:
            scores[r["index"]] = r["relevance_score"]
        return scores


class VoyageRerank(Reranker):
    """Voyage AI rerankers. Billed per token: the query counts once per document."""

    url = "https://api.voyageai.com/v1/rerank"

    def __init__(self, model: str):
        self.model, self.name = model, model

    def rerank(self, query, docs):
        body = {"model": self.model, "query": query[: self.max_query_chars], "documents": [d[: self.max_doc_chars] for d in docs]}
        out = _retry(lambda: _post_json(self.url, body, {"Authorization": "Bearer " + os.environ["VOYAGE_API_KEY"]}))
        _count("voyage:" + self.model, calls=1, tokens=out["usage"]["total_tokens"])
        scores = [0.0] * len(docs)
        for r in out["data"]:
            scores[r["index"]] = r["relevance_score"]
        return scores



class QwenReranker(Reranker):
    """Qwen3-Reranker run locally on CPU: P(yes) from the model's judge prompt."""

    instruction = "Given a search query, judge whether this issue or recorded decision of an engineering issue tracker answers it"

    def __init__(self, size: str = "0.6B", max_tokens: int = 2560):
        torch = _torch_threads()
        from transformers import AutoModelForCausalLM, AutoTokenizer

        self.torch = torch
        self.name = f"qwen3-reranker-{size.lower()}"
        mid = f"Qwen/Qwen3-Reranker-{size}"
        self.tok = AutoTokenizer.from_pretrained(mid, padding_side="left")
        self.model = AutoModelForCausalLM.from_pretrained(mid, dtype=torch.bfloat16).eval()
        self.max_tokens = max_tokens
        self.yes = self.tok.convert_tokens_to_ids("yes")
        self.no = self.tok.convert_tokens_to_ids("no")
        self.prefix = (
            "<|im_start|>system\nJudge whether the Document meets the requirements based on the Query and the Instruct "
            'provided. Note that the answer can only be "yes" or "no".<|im_end|>\n<|im_start|>user\n'
        )
        self.suffix = "<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n"
        # Same character caps as the hosted rerankers (the Reranker defaults), so all see equal input.

    def rerank(self, query, docs):
        t0 = time.time()
        torch = self.torch
        query = query[: self.max_query_chars]
        pre = self.tok.encode(self.prefix, add_special_tokens=False)
        suf = self.tok.encode(self.suffix, add_special_tokens=False)
        scores = []
        for start in range(0, len(docs), 8):
            bodies = [
                f"<Instruct>: {self.instruction}\n<Query>: {query}\n<Document>: {d[: self.max_doc_chars]}" for d in docs[start : start + 8]
            ]
            enc = self.tok(bodies, add_special_tokens=False, truncation=True, max_length=self.max_tokens - len(pre) - len(suf))
            ids = [pre + x + suf for x in enc["input_ids"]]
            batch = self.tok.pad({"input_ids": ids}, padding=True, return_tensors="pt")
            with torch.no_grad():
                logits = self.model(**batch).logits[:, -1, :]
            pair = torch.stack([logits[:, self.no], logits[:, self.yes]], dim=1).float()
            scores.extend(torch.softmax(pair, dim=1)[:, 1].tolist())
        _count("local:" + self.name, calls=1, docs=len(docs), cpu_seconds=time.time() - t0)
        return scores


EMBEDDERS = {
    "cohere-embed-v4": CohereEmbedV4,
    "titan-embed-v2": TitanEmbedV2,
    "gemini-embedding-001": GeminiEmbedding,
    "openai-3-large": OpenAIEmbedding3Large,
    "qwen3-embedding-0.6b": lambda: QwenEmbedding("0.6B"),
    "qwen3-embedding-4b": lambda: QwenEmbedding("4B"),
    "embed-v5.0-pro": lambda: CohereAPIEmbedding("embed-v5.0-pro", 2048),
    "embed-v5.0-fast": lambda: CohereAPIEmbedding("embed-v5.0-fast", 2048),
    "voyage-4-large": lambda: VoyageEmbedding("voyage-4-large"),
    "voyage-4": lambda: VoyageEmbedding("voyage-4"),
}

RERANKERS = {
    "cohere-rerank-3.5": CohereRerank35,
    "amazon-rerank-v1": AmazonRerankV1,
    "qwen3-reranker-0.6b": lambda: QwenReranker("0.6B"),
    "qwen3-reranker-4b": lambda: QwenReranker("4B"),
    "rerank-v4.0-pro": lambda: CohereAPIRerank("rerank-v4.0-pro"),
    "rerank-v4.0-fast": lambda: CohereAPIRerank("rerank-v4.0-fast"),
    "rerank-2.5": lambda: VoyageRerank("rerank-2.5"),
    "rerank-2.5-lite": lambda: VoyageRerank("rerank-2.5-lite"),
}
