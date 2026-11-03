"""How this repository reads a container image reference: the grammar of a reference, and the
repository a row of .github/ci-images.yaml names, with Docker Hub's implicit `library/` written
out.

check-ci-images.sh and mirror-ci-images.sh import it from the repository they work on, running
`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3` from that repository's root. One
module, because the gate and the copy must read a row the same way: a row the gate counts as the
library/golang row and the copy did not would escape the copy's Go check.
"""

import re

# ---------------------------------------------------------------- reference grammar
HOST = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?")
COMPONENT = re.compile(r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*")
TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9._-]{0,127}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
# The host spellings of Docker Hub, where a repository of one path element is under `library/`.
DOCKER_HUB_HOSTS = {"", "docker.io", "index.docker.io", "registry-1.docker.io"}


def split_reference(text: str) -> tuple[str, str, str, str] | None:
    """(host, path, tag, digest) for a string shaped like an image reference, else None. `host`
    and `tag` and `digest` are "" where the reference carries none."""
    if not text or any(character.isspace() for character in text) or "//" in text:
        return None
    body, at, digest = text.partition("@")
    if at and not DIGEST.fullmatch(digest):
        return None
    head, slash, last = body.rpartition("/")
    name, tag = body, ""
    if ":" in last:
        base, _, tag = last.partition(":")
        name = head + slash + base
        if not TAG.fullmatch(tag):
            return None
    parts = name.split("/")
    host = ""
    if len(parts) > 1 and ("." in parts[0] or ":" in parts[0] or parts[0] == "localhost"):
        host, parts = parts[0], parts[1:]
        if not HOST.fullmatch(host):
            return None
    if not parts or not all(COMPONENT.fullmatch(part) for part in parts):
        return None
    return host, "/".join(parts), tag, digest


def upstream_parts(upstream: str) -> tuple[str, str, str, str]:
    """(host, path, tag, digest) of a row's upstream, each "" where the reference carries none.

    `path` carries Docker Hub's implicit `library/`, which a reference may leave out:
    docker.io/golang and docker.io/library/golang are one image, and a rule that compared the
    text of the row would read the first as a repository no pin of this repository names. Every
    rule that compares a row's repository reads it from here, so both spellings answer alike."""
    host, path, tag, digest = split_reference(upstream) or ("", "", "", "")
    if path and host in DOCKER_HUB_HOSTS and "/" not in path:
        path = f"library/{path}"
    return host, path, tag, digest
