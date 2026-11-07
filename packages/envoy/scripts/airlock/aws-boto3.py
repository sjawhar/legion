"""k6's two boto3 steps against the stand-in AWS service: a chunk-signed upload, whose seed
signature covers the streaming payload constant and whose body carries a signature per chunk
chained from it, and a presigned URL. The airlock refuses both by name, and that refusal is what
this program reports.

The name has a hyphen in it on purpose: a file called boto3.py in the program's own directory is
what `import boto3` would find.

Usage: aws-boto3.py <endpoint URL> <out file>
"""

import hashlib
import hmac
import json
import os
import ssl
import sys
import urllib.request

import boto3
import botocore
from botocore.auth import AUTH_TYPE_MAPS, S3SigV4Auth

STREAMING = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
ENDPOINT, OUT = sys.argv[1], sys.argv[2]
BODY = b"k6 chunk-signed upload " * 1024
result = {"boto3": boto3.__version__, "botocore": botocore.__version__}


class ChunkSignedAuth(S3SigV4Auth):
    """The seed signature of a chunk-signed upload: the canonical request's payload hash is the
    streaming constant, which is what says every chunk carries a signature of its own."""

    def payload(self, request):
        return STREAMING


def sign(key, message):
    return hmac.new(key, message.encode(), hashlib.sha256).digest()


def text(value):
    return value.decode() if isinstance(value, bytes) else value


def frame(request, **_):
    """Replace the body with the aws-chunked framing a chunk-signed upload sends: each chunk's
    signature chained from the seed signature in the Authorization header."""
    authorization = text(request.headers["Authorization"])
    credential = authorization.split("Credential=")[1].split(",")[0]
    seed = authorization.split("Signature=")[1].strip()
    _, date, region, service, _ = credential.split("/")
    stamp = text(request.headers["X-Amz-Date"])
    scope = "/".join([date, region, service, "aws4_request"])
    secret = os.environ["AWS_SECRET_ACCESS_KEY"]
    signing = sign(sign(sign(sign(("AWS4" + secret).encode(), date), region), service), "aws4_request")
    empty = hashlib.sha256(b"").hexdigest()
    framed, previous = b"", seed
    for chunk in (BODY, b""):
        to_sign = "\n".join(
            ["AWS4-HMAC-SHA256-PAYLOAD", stamp, scope, previous, empty, hashlib.sha256(chunk).hexdigest()]
        )
        signature = hmac.new(signing, to_sign.encode(), hashlib.sha256).hexdigest()
        framed += f"{len(chunk):x};chunk-signature={signature}\r\n".encode() + chunk + b"\r\n"
        previous = signature
    request.body = framed
    request.headers["Content-Length"] = str(len(framed))
    result["chunks"], result["decoded_length"] = 2, len(BODY)


def chunk_headers(params, **_):
    params["headers"]["Content-Encoding"] = "aws-chunked"
    params["headers"]["x-amz-decoded-content-length"] = str(len(BODY))


def capture(response_dict=None, **_):
    if response_dict:
        body = response_dict.get("body") or b""
        result["chunk_answer"] = f"{response_dict.get('status_code')} {body[:400].decode(errors='replace')}".strip()


AUTH_TYPE_MAPS["s3v4"] = ChunkSignedAuth
signed = botocore.config.Config(signature_version="s3v4")
client = boto3.client("s3", endpoint_url=ENDPOINT, region_name="us-east-1", config=signed)
client.meta.events.register("before-call.s3.PutObject", chunk_headers)
client.meta.events.register("before-send.s3.PutObject", frame)
client.meta.events.register("response-received.s3.PutObject", capture)
try:
    client.put_object(Bucket="airlock-bucket", Key="k6-chunked", Body=BODY)
    result["chunk_signed"] = "the upload was not refused"
except Exception as refusal:  # noqa: BLE001 - the refusal is the result
    result["chunk_signed"] = str(refusal)

AUTH_TYPE_MAPS["s3v4"] = S3SigV4Auth
plain = boto3.client("s3", endpoint_url=ENDPOINT, region_name="us-east-1", config=signed)
url = plain.generate_presigned_url("get_object", Params={"Bucket": "airlock-bucket", "Key": "k6-100k.bin"})
result["presigned_query"] = sorted(part.split("=")[0] for part in url.split("?", 1)[1].split("&"))
opener = urllib.request.build_opener(
    urllib.request.ProxyHandler({"https": os.environ["HTTPS_PROXY"]}),
    urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=os.environ["AWS_CA_BUNDLE"])),
)
try:
    with opener.open(url, timeout=30) as answer:
        result["presigned"] = f"{answer.status} {answer.read(400).decode(errors='replace')}"
except Exception as refusal:  # noqa: BLE001 - the refusal is the result
    body = refusal.read(400).decode(errors="replace") if hasattr(refusal, "read") else ""
    result["presigned"] = f"{refusal} {body}".strip()

with open(OUT, "w") as answer:
    json.dump(result, answer)
