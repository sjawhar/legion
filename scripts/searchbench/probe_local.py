"""Measure local Qwen3 embedding/reranker throughput on real chunks, to pick the size that fits.

Usage: probe_local.py <data dir> <embedding size> <n chunks> [reranker size]
Prints seconds per chunk and the projected time to embed the whole corpus.
"""

import random
import sys
import time

import corpus
import providers as P

data, size, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
items, chunks = corpus.load(data)
random.seed(0)
sample = random.sample(chunks, n)
total_chars = sum(len(c.text) for c in chunks)
t = time.time()
e = P.QwenEmbedding(size)
print(f"load {time.time() - t:.1f}s")
t = time.time()
e.embed([c.text for c in sample], "document")
dt = time.time() - t
chars = sum(len(c.text) for c in sample)
print(f"embedding {size}: {n} chunks in {dt:.1f}s = {dt / n:.3f}s/chunk; corpus projection {dt / chars * total_chars / 3600:.2f} h")
if len(sys.argv) > 4:
    r = P.QwenReranker(sys.argv[4])
    docs = [c.text for c in sample[:32]]
    t = time.time()
    r.rerank("who decided how the merge queue authenticates?", docs)
    dt = time.time() - t
    print(f"reranker {sys.argv[4]}: 32 docs in {dt:.1f}s; 150 docs/query projection {dt / 32 * 150:.0f}s")
