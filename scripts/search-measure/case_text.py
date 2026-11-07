#!/usr/bin/env python3
"""How a case's query text is extracted from the fixture's own content.

A case is stored as a descriptor - which item, which version, which character range, and the
SHA-256 of the text that range yields - and never as the text itself, because this repository is
public and the text is Dispatch's content. Both the builder and the runner read the range through
the functions here, so the hash a descriptor carries is the hash of what the runner will send.

The rules are the ones the baseline's own case builder used, so that a case keeps the query it
was measured with:

  a document range  one line at a time: a fenced, heading, table, quote, html or directive line
                    drops out, a list or checkbox marker at the start of a line goes, what is
                    left is joined with single spaces, and the markup of a sentence (inline code,
                    links, emphasis) is reduced to its text;
  an ask range      the same reduction over the question's own text, with issue keys removed,
                    since a key in a query would find its issue for free.
"""

import hashlib
import re

URL_RE = re.compile(r"https?://\S+|dispatch://\S+")
KEY_RE = re.compile(r"\b[A-Z][A-Z0-9]{1,}-\d+\b")
SENTENCE_SPLIT_RE = re.compile(r"(?<=[.!?])\s+(?=[A-Z`\"'(])")
# A line's own markers, stripped in this order: a list bullet, then an ordered-list number, then
# a checkbox, so `- [ ] Do the thing` is `Do the thing`.
_LINE_MARKERS = (re.compile(r"^[-*+]\s+"), re.compile(r"^\d+\.\s+"), re.compile(r"^\[[ x]\]\s+"))


def clean_sentence(text):
    """A sentence's own words: no URL, no markup, single spaces."""
    text = URL_RE.sub(" ", text)
    text = re.sub(r"`([^`]*)`", r"\1", text)
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = re.sub(r"[*_#>]+", " ", text)
    return " ".join(text.split()).strip(" -–—*")


def extract_document_span(markdown, offset, length):
    """The query a document range yields."""
    kept = []
    for line in markdown[offset : offset + length].splitlines():
        stripped = line.strip()
        if _drops(stripped):
            continue
        kept.append(strip_markers(stripped))
    return clean_sentence(" ".join(" ".join(kept).split()))


def extract_ask_span(question, offset, length):
    """The query an ask's question range yields."""
    text = clean_sentence(" ".join(question[offset : offset + length].split()))
    return " ".join(KEY_RE.sub(" ", text).split())


def _drops(stripped):
    """A line that carries no prose of its own."""
    return (
        stripped.startswith(("```", "~~~", ":::", "#", "|", ">"))
        or (stripped.startswith("<") and stripped.endswith(">"))
    )


def strip_markers(stripped):
    """A line without its list, number or checkbox marker."""
    for marker in _LINE_MARKERS:
        stripped = marker.sub("", stripped)
    return stripped


def _marker_width(stripped):
    """How many characters strip_markers takes off the front of stripped."""
    return len(stripped) - len(strip_markers(stripped))



def document_sentence_spans(markdown):
    """Every sentence of a document's prose, as (offset, length, query).

    The offsets are into the markdown itself, so a descriptor built from one can be read back by
    extract_document_span. A paragraph ends at a blank line; a line the rules drop ends nothing,
    which is what the baseline's builder did.
    """
    paragraphs, current = [], []
    for start, line in _lines(markdown):
        stripped = line.strip()
        if _drops(stripped):
            continue
        lead = line.index(stripped) if stripped else 0
        width = _marker_width(stripped)
        text = stripped[width:]
        if not text:
            if current:
                paragraphs.append(current)
                current = []
            continue
        current.append((start + lead + width, text))
    if current:
        paragraphs.append(current)

    spans = []
    for paragraph in paragraphs:
        joined, positions = _join(paragraph)
        for first, last in _sentence_bounds(joined):
            offset = positions[first]
            end = positions[last - 1] + 1
            query = clean_sentence(joined[first:last])
            if query:
                spans.append((offset, end - offset, query))
    return spans


def _lines(text):
    """Each line with the offset it starts at."""
    start = 0
    for line in text.splitlines(keepends=True):
        yield start, line.rstrip("\r\n")
        start += len(line)


def _join(paragraph):
    """A paragraph's lines as one whitespace-collapsed string, with each character's own offset
    in the markdown."""
    pieces, positions = [], []
    for start, text in paragraph:
        if pieces:
            pieces.append(" ")
            positions.append(positions[-1] + 1)
        for index, character in enumerate(text):
            pieces.append(character)
            positions.append(start + index)
    joined = "".join(pieces)
    collapsed, kept = [], []
    index = 0
    while index < len(joined):
        if joined[index].isspace():
            collapsed.append(" ")
            kept.append(positions[index])
            while index < len(joined) and joined[index].isspace():
                index += 1
            continue
        collapsed.append(joined[index])
        kept.append(positions[index])
        index += 1
    flat = "".join(collapsed)
    text = flat.strip()
    offset = len(flat) - len(flat.lstrip())
    return text, kept[offset : offset + len(text)]


def _sentence_bounds(text):
    """Each sentence's half-open bounds in text."""
    bounds, start = [], 0
    for match in SENTENCE_SPLIT_RE.finditer(text):
        bounds.append((start, match.start()))
        start = match.end()
    bounds.append((start, len(text)))
    return [(first, last) for first, last in bounds if last > first]


def digest(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()
