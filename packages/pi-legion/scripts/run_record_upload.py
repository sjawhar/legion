#!/usr/bin/env python3
"""Copies the run record spool to the operator's bucket, one write-once object per line.

The run record hook (`src/run-record.ts`) and the deployment's record builder append one JSON line
per run start and end to `<spool>/<YYYY-MM-DD>/<session id>.jsonl`. This uploader puts each
complete line in Cloud Storage as

    gs://<bucket>/runs/v1/<project>/<host>/<YYYY-MM-DD>/<session_id>/<run_id>.<kind>.json

with `ifGenerationMatch=0`, so an object is written once and never replaced; a 412 means the
object is already there and counts as uploaded. `<project>` and `<host>` are this uploader's own
configuration, the day is the spool day directory, and `session_id`, `run_id` and `kind` come from
the line. A line that is not a JSON object of schema `run-record/1`, or whose ids are not names
the bucket takes, is skipped and counted. A trailing line with no newline yet is left for the next
pass.

Run it as root (README.md, "Run records"). The spool belongs to the panes, so root never opens
it: the `read` half of this script runs as the spool directory's owner (`setpriv --reuid
--regid --clear-groups` when the caller is someone else) and streams the lines to the root half,
which holds the cloud identity, uploads the bytes it is given, and keeps in its own state
directory how far into each spool file it has uploaded. Standard library only.
"""

import argparse
import base64
import fcntl
import http.client
import json
import os
import re
import shlex
import socket
import stat
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request

ID = re.compile(r"[A-Za-z0-9._-]{1,128}")
DAY = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}")
SPOOL_FILE = re.compile(r"([0-9]{4}-[0-9]{2}-[0-9]{2})/([A-Za-z0-9._-]{1,128})\.jsonl")
SCHEMA = "run-record/1"
KINDS = ("start", "end")
DEFAULT_ENDPOINT = "https://storage.googleapis.com"
DEFAULT_METADATA_URL = (
    "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
)
DEFAULT_STATE_DIR = "/var/lib/legion-run-records"
DEFAULT_MAX_LINE_BYTES = 4 * 1024 * 1024


class UploadError(Exception):
    pass


def is_id(value):
    return isinstance(value, str) and ID.fullmatch(value) is not None and value not in (".", "..")


def object_name(project, host, day, data):
    """The object a spool line goes to, or None when the line is not one to upload."""
    try:
        record = json.loads(data)
    except ValueError:
        return None
    if not isinstance(record, dict) or record.get("schema") != SCHEMA:
        return None
    kind = record.get("kind")
    session_id = record.get("session_id")
    run_id = record.get("run_id")
    if kind not in KINDS or not is_id(session_id) or not is_id(run_id):
        return None
    return f"runs/v1/{project}/{host}/{day}/{session_id}/{run_id}.{kind}.json"


# The reader: the half that runs as the spool's owner.


def emit(out, record):
    out.write(json.dumps(record, separators=(",", ":")).encode() + b"\n")


def read_spool(spool_dir, state, max_line_bytes, out):
    """Streams every spool file's complete lines after the offset `state` records for it.

    A file whose inode changed, or that is shorter than its offset, is read from the start again.
    Day directories and files are taken only as real directories and regular files, never through
    a symbolic link.
    """
    with os.scandir(spool_dir) as days:
        day_names = sorted(
            entry.name
            for entry in days
            if DAY.fullmatch(entry.name) and entry.is_dir(follow_symlinks=False)
        )
    for day in day_names:
        day_dir = os.path.join(spool_dir, day)
        with os.scandir(day_dir) as files:
            names = sorted(
                entry.name
                for entry in files
                if entry.name.endswith(".jsonl")
                and ID.fullmatch(entry.name[: -len(".jsonl")])
                and entry.is_file(follow_symlinks=False)
            )
        for name in names:
            rel = f"{day}/{name}"
            read_file(os.path.join(day_dir, name), rel, day, state.get(rel), max_line_bytes, out)
    emit(out, {"type": "done"})


def read_file(file_path, rel, day, previous, max_line_bytes, out):
    try:
        fd = os.open(file_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError:
        return
    with os.fdopen(fd, "rb") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode):
            return
        start = 0
        if (
            isinstance(previous, dict)
            and previous.get("ino") == info.st_ino
            and isinstance(previous.get("offset"), int)
            and 0 <= previous["offset"] <= info.st_size
        ):
            start = previous["offset"]
        emit(out, {"type": "file", "file": rel, "ino": info.st_ino, "start": start})
        handle.seek(start)
        position = start
        pending = 0
        while True:
            chunk = handle.readline(max_line_bytes + 1)
            if not chunk:
                break
            if chunk.endswith(b"\n"):
                position += len(chunk)
                data = base64.b64encode(chunk[:-1]).decode()
                emit(out, {"type": "line", "file": rel, "day": day, "end": position, "data": data})
                continue
            if len(chunk) <= max_line_bytes:
                pending = len(chunk)
                break
            # A line longer than the limit: skipped whole once its newline arrives.
            skipped = len(chunk)
            while True:
                more = handle.readline(max_line_bytes + 1)
                if not more:
                    pending = skipped
                    break
                skipped += len(more)
                if more.endswith(b"\n"):
                    position += skipped
                    emit(out, {"type": "oversized", "file": rel, "end": position})
                    break
            if pending:
                break
        emit(out, {"type": "eof", "file": rel, "pending": pending})


def run_reader(args):
    request = json.load(sys.stdin)
    out = sys.stdout.buffer
    read_spool(args.spool_dir, request.get("files", {}), args.max_line_bytes, out)
    out.flush()
    return 0


# The uploader: the half that runs as root and holds the cloud identity.


class MetadataToken:
    """An access token from the VM's metadata server, reused until shortly before it expires."""

    def __init__(self, url):
        self.url = url
        self.token = None
        self.expires = 0.0

    def get(self):
        if self.token is not None and time.monotonic() < self.expires:
            return self.token
        request = urllib.request.Request(self.url, headers={"Metadata-Flavor": "Google"})
        with urllib.request.urlopen(request, timeout=30) as response:
            body = json.load(response)
        self.token = body["access_token"]
        self.expires = time.monotonic() + max(0, int(body.get("expires_in", 0)) - 60)
        return self.token

    def invalidate(self):
        self.token = None


class CommandToken:
    """An access token printed by an operator's command, reused for the pass."""

    def __init__(self, command):
        self.argv = shlex.split(command)
        self.token = None

    def get(self):
        if self.token is None:
            result = subprocess.run(self.argv, stdout=subprocess.PIPE, check=True, timeout=60)
            self.token = result.stdout.decode().strip()
            if not self.token:
                raise UploadError("the token command printed no token")
        return self.token

    def invalidate(self):
        self.token = None


class Bucket:
    """Write-once uploads to one bucket over the JSON API, on one kept-alive connection."""

    def __init__(self, endpoint, bucket, tokens):
        parts = urllib.parse.urlsplit(endpoint)
        if parts.scheme not in ("https", "http") or not parts.netloc:
            raise UploadError(f"the endpoint {endpoint!r} is not an http(s) URL")
        self.scheme = parts.scheme
        self.netloc = parts.netloc
        self.prefix = parts.path.rstrip("/")
        self.bucket = bucket
        self.tokens = tokens
        self.connection = None

    def close(self):
        if self.connection is not None:
            self.connection.close()
            self.connection = None

    def _post(self, path, data):
        for attempt in (1, 2):
            if self.connection is None:
                kind = http.client.HTTPSConnection if self.scheme == "https" else http.client.HTTPConnection
                self.connection = kind(self.netloc, timeout=60)
            headers = {
                "Authorization": f"Bearer {self.tokens.get()}",
                "Content-Type": "application/json",
                "Content-Length": str(len(data)),
            }
            try:
                self.connection.request("POST", path, body=data, headers=headers)
                response = self.connection.getresponse()
                response.read()
                return response.status
            except (http.client.HTTPException, OSError) as error:
                # A kept-alive connection the server closed: one fresh connection, then give up.
                self.close()
                if attempt == 2:
                    raise UploadError(f"the upload failed: {type(error).__name__}") from None
        raise AssertionError("unreachable")

    def create(self, name, data):
        """Writes the object once: "created", or "exists" when it is already there."""
        query = urllib.parse.urlencode({"uploadType": "media", "name": name, "ifGenerationMatch": "0"})
        path = f"{self.prefix}/upload/storage/v1/b/{urllib.parse.quote(self.bucket, safe='')}/o?{query}"
        status = self._post(path, data)
        if status == 401:
            self.tokens.invalidate()
            status = self._post(path, data)
        if status in (200, 201):
            return "created"
        if status == 412:
            return "exists"
        raise UploadError(f"the upload answered HTTP {status}")


def load_state(state_file):
    try:
        with open(state_file, encoding="utf-8") as handle:
            state = json.load(handle)
    except FileNotFoundError:
        return {}
    files = state.get("files") if isinstance(state, dict) else None
    return files if isinstance(files, dict) else {}


def save_state(state_dir, state_file, files):
    descriptor, temporary = tempfile.mkstemp(dir=state_dir, prefix=".state-")
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump({"version": 1, "files": files}, handle, sort_keys=True)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, state_file)
    except BaseException:
        os.unlink(temporary)
        raise


def reader_command(args):
    command = [
        sys.executable,
        "-I",
        "-B",
        os.path.abspath(__file__),
        "read",
        "--spool-dir",
        args.spool_dir,
        "--max-line-bytes",
        str(args.max_line_bytes),
    ]
    info = os.lstat(args.spool_dir)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise UploadError(f"the spool {args.spool_dir} is not a directory")
    uid = info.st_uid if args.reader_uid is None else args.reader_uid
    gid = info.st_gid if args.reader_gid is None else args.reader_gid
    if os.geteuid() == uid:
        return command
    return ["setpriv", f"--reuid={uid}", f"--regid={gid}", "--clear-groups", *command]


def run_upload(args):
    for flag, value in (("--project", args.project), ("--host", args.host)):
        if not is_id(value):
            print(f"run-records: {flag} {value!r} is not a name the bucket takes", file=sys.stderr)
            return 2
    os.makedirs(args.state_dir, mode=0o700, exist_ok=True)
    lock = open(os.path.join(args.state_dir, "lock"), "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        print("run-records: another pass holds the state lock; nothing done")
        return 0
    state_file = os.path.join(args.state_dir, "state.json")
    previous = load_state(state_file)
    if args.token_command:
        tokens = CommandToken(args.token_command)
    else:
        tokens = MetadataToken(args.metadata_url)
    bucket = Bucket(args.endpoint, args.bucket, tokens)
    counts = {"uploaded": 0, "already": 0, "invalid": 0, "errors": 0, "incomplete": 0}
    files = {}
    complete = False
    current = None
    try:
        reader = subprocess.Popen(reader_command(args), stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    except (OSError, UploadError) as error:
        print(f"run-records: the spool reader did not start: {error}", file=sys.stderr)
        return 1
    try:
        reader.stdin.write(json.dumps({"files": previous}).encode())
        reader.stdin.close()
        for raw in reader.stdout:
            record = json.loads(raw)
            kind = record.get("type")
            if kind == "done":
                complete = True
                continue
            rel = record.get("file")
            match = SPOOL_FILE.fullmatch(rel) if isinstance(rel, str) else None
            if match is None:
                raise UploadError("the spool reader named a file that is not a spool file")
            if kind == "file":
                current = {"ino": record["ino"], "offset": record["start"], "stalled": False}
                files[rel] = {"ino": current["ino"], "offset": current["offset"]}
                continue
            if current is None or current.get("stalled"):
                continue
            if kind == "eof":
                if record.get("pending"):
                    counts["incomplete"] += 1
                current = None
                continue
            if kind == "oversized":
                counts["invalid"] += 1
            elif kind == "line":
                data = base64.b64decode(record["data"])
                name = object_name(args.project, args.host, match.group(1), data)
                if name is None:
                    counts["invalid"] += 1
                elif args.dry_run:
                    print(f"gs://{args.bucket}/{name}")
                    counts["uploaded"] += 1
                else:
                    try:
                        outcome = bucket.create(name, data)
                    except (UploadError, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
                        print(f"run-records: {rel}: {error}", file=sys.stderr)
                        counts["errors"] += 1
                        current["stalled"] = True
                        continue
                    counts["uploaded" if outcome == "created" else "already"] += 1
            else:
                raise UploadError(f"the spool reader sent a record of type {kind!r}")
            current["offset"] = record["end"]
            files[rel]["offset"] = record["end"]
    except (UploadError, ValueError, KeyError) as error:
        print(f"run-records: reading the spool failed: {error}", file=sys.stderr)
        counts["errors"] += 1
    finally:
        bucket.close()
        if reader.stdout is not None:
            reader.stdout.close()
        status = reader.wait()
    if status != 0:
        print(f"run-records: the spool reader exited {status}", file=sys.stderr)
        counts["errors"] += 1
        complete = False
    if not args.dry_run:
        # A pass that read the whole spool forgets files no longer in it; any other keeps them.
        merged = files if complete else {**previous, **files}
        save_state(args.state_dir, state_file, merged)
    verb = "would upload" if args.dry_run else "uploaded"
    print(
        f"run-records: {verb}={counts['uploaded']} already={counts['already']} invalid={counts['invalid']}"
        f" incomplete_files={counts['incomplete']} errors={counts['errors']}"
    )
    return 1 if counts["errors"] else 0


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    commands = parser.add_subparsers(dest="command", required=True)
    upload = commands.add_parser("upload", help="upload the spool's new complete lines (run as root)")
    upload.add_argument(
        "--spool-dir", required=True, help="the spool root the hook's RUN_RECORD_DIR names on this host"
    )
    upload.add_argument("--bucket", required=True, help="the operator's bucket, without gs://")
    upload.add_argument("--project", required=True, help="the project segment of every object name")
    upload.add_argument(
        "--host", default=socket.gethostname(), help="the host segment of every object name (default: this host's name)"
    )
    upload.add_argument(
        "--state-dir", default=DEFAULT_STATE_DIR, help=f"root's record of what is uploaded (default {DEFAULT_STATE_DIR})"
    )
    upload.add_argument("--dry-run", action="store_true", help="upload nothing, record nothing, print each object name")
    upload.add_argument("--token-command", help="a command that prints an access token, in place of the metadata server")
    upload.add_argument("--metadata-url", default=DEFAULT_METADATA_URL, help="the metadata server's token URL")
    upload.add_argument("--endpoint", default=DEFAULT_ENDPOINT, help=f"the Cloud Storage endpoint (default {DEFAULT_ENDPOINT})")
    upload.add_argument("--reader-uid", type=int, help="the uid the spool is read as (default: the spool directory's owner)")
    upload.add_argument("--reader-gid", type=int, help="the gid the spool is read as (default: the spool directory's group)")
    upload.add_argument("--max-line-bytes", type=int, default=DEFAULT_MAX_LINE_BYTES, help=argparse.SUPPRESS)
    read = commands.add_parser("read", help=argparse.SUPPRESS)
    read.add_argument("--spool-dir", required=True)
    read.add_argument("--max-line-bytes", type=int, default=DEFAULT_MAX_LINE_BYTES)
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)
    if args.command == "read":
        return run_reader(args)
    return run_upload(args)


if __name__ == "__main__":
    sys.exit(main())
