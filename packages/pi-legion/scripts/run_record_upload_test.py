"""Tests of run_record_upload.py against a local stand-in for Cloud Storage and the metadata server.

Each test runs the script as the operator would, in a subprocess, on a spool of synthetic lines.
`scripts/run-record-upload.test.ts` runs this file under `bun test`.
"""

import http.server
import json
import os
import shlex
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.parse

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "run_record_upload.py")
BUCKET = "example-bucket"


class FakeCloud(http.server.ThreadingHTTPServer):
    """Cloud Storage's media upload with ifGenerationMatch, and the metadata server's token."""

    def __init__(self):
        super().__init__(("127.0.0.1", 0), FakeCloudHandler)
        self.objects = {}
        self.uploads = []
        self.token_requests = []
        self.fail_once = set()
        self.lock = threading.Lock()

    @property
    def base(self):
        return f"http://127.0.0.1:{self.server_address[1]}"


class FakeCloudHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):
        pass

    def answer(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        with self.server.lock:
            self.server.token_requests.append(self.headers.get("Metadata-Flavor"))
        if self.path != "/token" or self.headers.get("Metadata-Flavor") != "Google":
            self.answer(403, {})
            return
        self.answer(200, {"access_token": "meta-token", "expires_in": 3600, "token_type": "Bearer"})

    def do_POST(self):
        parts = urllib.parse.urlsplit(self.path)
        query = dict(urllib.parse.parse_qsl(parts.query))
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        name = query.get("name", "")
        with self.server.lock:
            self.server.uploads.append(
                {
                    "path": parts.path,
                    "query": query,
                    "authorization": self.headers.get("Authorization"),
                    "content_type": self.headers.get("Content-Type"),
                    "body": body,
                }
            )
            if name in self.server.fail_once:
                self.server.fail_once.discard(name)
                status = 503
            elif query.get("ifGenerationMatch") == "0" and name in self.server.objects:
                status = 412
            else:
                self.server.objects[name] = body
                status = 200
        self.answer(status, {"name": name})


def record(kind, session_id, run_id, **fields):
    return {"schema": "run-record/1", "kind": kind, "run_id": run_id, "session_id": session_id, **fields}


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


class UploaderTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory(prefix="run-record-upload-")
        self.spool = os.path.join(self.root.name, "spool")
        self.state = os.path.join(self.root.name, "state")
        os.makedirs(self.spool)
        self.cloud = FakeCloud()
        self.thread = threading.Thread(target=self.cloud.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.cloud.shutdown()
        self.cloud.server_close()
        self.root.cleanup()

    def append(self, day, session_id, data):
        directory = os.path.join(self.spool, day)
        os.makedirs(directory, exist_ok=True)
        with open(os.path.join(directory, f"{session_id}.jsonl"), "ab") as handle:
            handle.write(data)

    def run_uploader(self, *extra):
        command = [
            sys.executable,
            SCRIPT,
            "upload",
            "--spool-dir",
            self.spool,
            "--bucket",
            BUCKET,
            "--project",
            "acme",
            "--host",
            "example-host-a",
            "--state-dir",
            self.state,
            "--endpoint",
            self.cloud.base,
            "--metadata-url",
            f"{self.cloud.base}/token",
            *extra,
        ]
        result = subprocess.run(command, capture_output=True, timeout=60)
        return result.returncode, result.stdout.decode(), result.stderr.decode()

    def summary(self, stdout):
        return stdout.strip().splitlines()[-1]

    def uploaded_names(self):
        return [upload["query"]["name"] for upload in self.cloud.uploads]

    def test_each_complete_line_goes_up_once_as_a_write_once_object_under_its_name(self):
        start = encoded(record("start", "ses-a", "run-1", host="other-host", project="other"))
        end = encoded(record("end", "ses-a", "run-1", end_reason="agent_end"))
        other = encoded(record("start", "ses-b", "run-2"))
        self.append("2026-10-29", "ses-a", start + b"\n" + end + b"\n")
        self.append("2026-10-30", "ses-b", other + b"\n")

        code, stdout, stderr = self.run_uploader()

        self.assertEqual(code, 0, stderr)
        self.assertEqual(
            self.summary(stdout), "run-records: uploaded=3 already=0 invalid=0 incomplete_files=0 errors=0"
        )
        # The project and host are the uploader's, whatever the line says.
        self.assertEqual(
            sorted(self.uploaded_names()),
            [
                "runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.end.json",
                "runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.start.json",
                "runs/v1/acme/example-host-a/2026-10-30/ses-b/run-2.start.json",
            ],
        )
        upload = next(u for u in self.cloud.uploads if u["query"]["name"].endswith("run-1.start.json"))
        self.assertEqual(upload["path"], f"/upload/storage/v1/b/{BUCKET}/o")
        self.assertEqual(upload["query"]["uploadType"], "media")
        self.assertEqual(upload["query"]["ifGenerationMatch"], "0")
        self.assertEqual(upload["authorization"], "Bearer meta-token")
        self.assertEqual(upload["content_type"], "application/json")
        # The object is the line's bytes, without its newline.
        self.assertEqual(upload["body"], start)
        self.assertEqual(self.cloud.token_requests, ["Google"])
        self.assertEqual(os.stat(self.state).st_mode & 0o777, 0o700)

        # The state records what went up: the next pass sends nothing.
        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertEqual(len(self.cloud.uploads), 3)
        self.assertIn("uploaded=0 already=0", self.summary(stdout))

    def test_an_object_already_in_the_bucket_counts_as_uploaded_and_is_not_replaced(self):
        line = encoded(record("start", "ses-a", "run-1"))
        name = "runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.start.json"
        self.cloud.objects[name] = b"the first upload"
        self.append("2026-10-29", "ses-a", line + b"\n")

        code, stdout, stderr = self.run_uploader()

        self.assertEqual(code, 0, stderr)
        self.assertIn("uploaded=0 already=1", self.summary(stdout))
        self.assertEqual(self.cloud.objects[name], b"the first upload")

    def test_a_trailing_line_without_its_newline_waits_for_the_next_pass(self):
        first = encoded(record("start", "ses-a", "run-1"))
        second = encoded(record("end", "ses-a", "run-1"))
        self.append("2026-10-29", "ses-a", first + b"\n" + second[:20])

        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertEqual(
            self.summary(stdout), "run-records: uploaded=1 already=0 invalid=0 incomplete_files=1 errors=0"
        )

        self.append("2026-10-29", "ses-a", second[20:] + b"\n")
        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertIn("uploaded=1 already=0 invalid=0 incomplete_files=0", self.summary(stdout))
        self.assertEqual(self.cloud.uploads[-1]["body"], second)

    def test_a_line_that_names_no_valid_object_is_skipped_and_counted_once(self):
        good = encoded(record("end", "ses-a", "run-1"))
        bad = [
            b"",
            b"not json",
            b"[1, 2]",
            encoded({**record("start", "ses-a", "run-2"), "schema": "run-record/2"}),
            encoded(record("middle", "ses-a", "run-3")),
            encoded(record("start", "../ses-a", "run-4")),
            encoded({"schema": "run-record/1", "kind": "start", "session_id": "ses-a"}),
            encoded(record("start", "ses-a", "..")),
        ]
        self.append("2026-10-29", "ses-a", b"\n".join([*bad, good]) + b"\n")

        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertEqual(
            self.summary(stdout), "run-records: uploaded=1 already=0 invalid=8 incomplete_files=0 errors=0"
        )
        self.assertEqual(self.uploaded_names(), ["runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.end.json"])

        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertIn("uploaded=0 already=0 invalid=0", self.summary(stdout))

    def test_a_line_over_the_limit_is_skipped_once_its_newline_arrives(self):
        long = encoded(record("start", "ses-a", "run-1", padding="x" * 400))
        short = encoded(record("end", "ses-a", "run-1"))
        self.append("2026-10-29", "ses-a", long + b"\n" + short + b"\n")

        code, stdout, stderr = self.run_uploader("--max-line-bytes", "200")

        self.assertEqual(code, 0, stderr)
        self.assertIn("uploaded=1 already=0 invalid=1", self.summary(stdout))
        self.assertEqual(self.uploaded_names(), ["runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.end.json"])

    def test_a_dry_run_uploads_nothing_records_nothing_and_prints_each_object(self):
        self.append(
            "2026-10-29",
            "ses-a",
            encoded(record("start", "ses-a", "run-1")) + b"\n" + encoded(record("end", "ses-a", "run-1")) + b"\n",
        )

        code, stdout, stderr = self.run_uploader("--dry-run")

        self.assertEqual(code, 0, stderr)
        self.assertEqual(
            stdout.strip().splitlines(),
            [
                f"gs://{BUCKET}/runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.start.json",
                f"gs://{BUCKET}/runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.end.json",
                "run-records: would upload=2 already=0 invalid=0 incomplete_files=0 errors=0",
            ],
        )
        self.assertEqual(self.cloud.uploads, [])
        self.assertEqual(self.cloud.token_requests, [])
        self.assertFalse(os.path.exists(os.path.join(self.state, "state.json")))

        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertIn("uploaded=2", self.summary(stdout))

    def test_a_failed_upload_stops_its_file_there_and_the_next_pass_resumes_from_it(self):
        lines = [encoded(record("start", "ses-a", f"run-{n}")) for n in (1, 2, 3)]
        self.append("2026-10-29", "ses-a", b"".join(line + b"\n" for line in lines))
        self.append("2026-10-29", "ses-b", encoded(record("start", "ses-b", "run-9")) + b"\n")
        self.cloud.fail_once.add("runs/v1/acme/example-host-a/2026-10-29/ses-a/run-2.start.json")

        code, stdout, stderr = self.run_uploader()

        self.assertEqual(code, 1)
        self.assertIn("HTTP 503", stderr)
        self.assertEqual(
            self.summary(stdout), "run-records: uploaded=2 already=0 invalid=0 incomplete_files=0 errors=1"
        )
        self.assertEqual(
            sorted(self.uploaded_names()),
            [
                "runs/v1/acme/example-host-a/2026-10-29/ses-a/run-1.start.json",
                "runs/v1/acme/example-host-a/2026-10-29/ses-a/run-2.start.json",
                "runs/v1/acme/example-host-a/2026-10-29/ses-b/run-9.start.json",
            ],
        )

        before = len(self.cloud.uploads)
        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertEqual(
            [upload["query"]["name"] for upload in self.cloud.uploads[before:]],
            [
                "runs/v1/acme/example-host-a/2026-10-29/ses-a/run-2.start.json",
                "runs/v1/acme/example-host-a/2026-10-29/ses-a/run-3.start.json",
            ],
        )

    def test_a_token_command_replaces_the_metadata_server(self):
        self.append("2026-10-29", "ses-a", encoded(record("start", "ses-a", "run-1")) + b"\n")
        command = shlex.join([sys.executable, "-c", "print('command-token')"])

        code, stdout, stderr = self.run_uploader("--token-command", command)

        self.assertEqual(code, 0, stderr)
        self.assertEqual([upload["authorization"] for upload in self.cloud.uploads], ["Bearer command-token"])
        self.assertEqual(self.cloud.token_requests, [])

    def test_a_symbolic_link_in_the_spool_is_never_followed(self):
        outside = os.path.join(self.root.name, "outside.jsonl")
        with open(outside, "wb") as handle:
            handle.write(encoded(record("start", "ses-x", "run-1")) + b"\n")
        os.makedirs(os.path.join(self.spool, "2026-10-29"))
        os.symlink(outside, os.path.join(self.spool, "2026-10-29", "ses-x.jsonl"))
        os.symlink(self.root.name, os.path.join(self.spool, "2026-10-30"))

        code, stdout, stderr = self.run_uploader()

        self.assertEqual(code, 0, stderr)
        self.assertEqual(self.cloud.uploads, [])

    def test_a_replaced_spool_file_is_read_from_its_start_again(self):
        line = encoded(record("start", "ses-a", "run-1"))
        self.append("2026-10-29", "ses-a", line + b"\n")
        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)

        path = os.path.join(self.spool, "2026-10-29", "ses-a.jsonl")
        replacement = path + ".new"
        with open(replacement, "wb") as handle:
            handle.write(line + b"\n" + encoded(record("end", "ses-a", "run-1")) + b"\n")
        os.replace(replacement, path)

        code, stdout, stderr = self.run_uploader()
        self.assertEqual(code, 0, stderr)
        self.assertIn("uploaded=1 already=1", self.summary(stdout))

    def test_a_project_or_host_the_bucket_cannot_take_is_refused_before_anything_runs(self):
        self.append("2026-10-29", "ses-a", encoded(record("start", "ses-a", "run-1")) + b"\n")
        command = [sys.executable, SCRIPT, "upload", "--spool-dir", self.spool, "--bucket", BUCKET]
        for extra in (["--project", "acme/other", "--host", "h"], ["--project", "acme", "--host", ".."]):
            result = subprocess.run([*command, *extra, "--state-dir", self.state], capture_output=True, timeout=60)
            self.assertEqual(result.returncode, 2, extra)
        self.assertEqual(self.cloud.uploads, [])
        self.assertFalse(os.path.exists(self.state))

    def test_the_bucket_is_a_required_input(self):
        result = subprocess.run(
            [sys.executable, SCRIPT, "upload", "--spool-dir", self.spool, "--project", "acme"],
            capture_output=True,
            timeout=60,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("--bucket", result.stderr.decode())


if __name__ == "__main__":
    unittest.main()
