#!/usr/bin/env python3
"""The registration endpoint nodes announce themselves to
(docs/private-cloud/first-contact.md), protocol 2: a node sends its CA,
name and address - never a key -, and gets the fleet's trust once it's
admitted. Python's standard library only: any language that serves
HTTPS does the same.

    registration-server.py --listen 0.0.0.0:8443 --fleet FLEET-DIR \
        --cert registration.crt --key registration.key --state DIR

- A token listed in DIR/tokens (one per line), given to a machine the
  orchestrator created, admits its node at once - once.
- Any other announcement waits: DIR/nodes/<id>.json says what it is;
  creating DIR/approved/<id> admits it, and the node gets the trust at
  its next poll (every 15 seconds).
- DIR/nodes/<id>.json is the inventory: the node's name, its address,
  its CA - what the orchestrator checks it with.

A node falls back to protocol 1 - announcing with a key - only when told
409: this server never asks for one.
"""
import argparse
import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import ssl
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def write_json(path, value):
    """Replaces path with value as JSON, whole or not at all."""
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path))
    with os.fdopen(fd, "w") as f:
        json.dump(value, f, indent=2)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def is_ca(pem):
    """The certificate is a CA's: openssl prints its basic constraints."""
    try:
        out = subprocess.run(["openssl", "x509", "-noout", "-ext", "basicConstraints"],
                             input=pem.encode(), capture_output=True, check=True).stdout
    except subprocess.CalledProcessError:
        return False
    return b"CA:TRUE" in out


class Registration(BaseHTTPRequestHandler):
    def answer(self, status, body=None):
        data = json.dumps(body).encode() if body is not None else b""
        self.send_response(status)
        if data:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def trust(self):
        """The fleet's trust: its root, and the bundle - the signed file's
        bytes, base64."""
        fleet = self.server.fleet
        with open(os.path.join(fleet, "root.crt")) as f:
            root = f.read()
        with open(os.path.join(fleet, "bundle.json"), "rb") as f:
            bundle = f.read()
        return {"root_cert": root, "bundle": base64.b64encode(bundle).decode()}

    def take_token(self, token):
        """Consumes token from DIR/tokens: true when it was there."""
        path = os.path.join(self.server.state, "tokens")
        with self.server.lock:
            try:
                with open(path) as f:
                    tokens = f.read().split()
            except FileNotFoundError:
                return False
            if not token or token not in tokens:
                return False
            tokens.remove(token)
            with open(path + ".tmp", "w") as f:
                f.write("".join(t + "\n" for t in tokens))
            os.replace(path + ".tmp", path)
            return True

    def do_POST(self):
        if self.path != "/register":
            return self.answer(404)
        try:
            length = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(min(length, 1 << 20)))
        except (ValueError, json.JSONDecodeError):
            return self.answer(400, {"error": "not JSON"})
        if req.get("protocol", 1) < 2:
            # A node sends protocol 2 first; protocol 1 carries a key.
            return self.answer(400, {"error": "protocol 2 only"})
        name, address, ca = req.get("name", ""), req.get("address", ""), req.get("ca_cert_pem", "")
        secret = req.get("poll_secret", "")
        if not name or not address or len(secret) < 32 or not is_ca(ca):
            return self.answer(400, {"error": "name, address, a CA certificate and a poll secret are required"})
        node_id = secrets.token_hex(16)
        admitted = self.take_token(req.get("registration_token", ""))
        write_json(os.path.join(self.server.state, "nodes", node_id + ".json"), {
            "id": node_id, "name": name, "address": address, "ca_cert_pem": ca,
            "admitted": admitted,
            # Only the hash: whoever reads the state can't poll as the node.
            "poll_secret_sha256": hashlib.sha256(secret.encode()).hexdigest(),
        })
        if admitted:
            open(os.path.join(self.server.state, "approved", node_id), "w").close()
            return self.answer(201, {"id": node_id, "admitted": True, "trust": self.trust()})
        return self.answer(201, {"id": node_id, "admitted": False})

    def do_GET(self):
        m = re.fullmatch(r"/register/([0-9a-f]{32})", self.path)
        if not m:
            return self.answer(404)
        node_id = m.group(1)
        try:
            with open(os.path.join(self.server.state, "nodes", node_id + ".json")) as f:
                node = json.load(f)
        except FileNotFoundError:
            return self.answer(404)  # unknown, or forgotten: the node announces again
        auth = self.headers.get("Authorization", "")
        given = hashlib.sha256(auth.removeprefix("Bearer ").encode()).hexdigest()
        if not auth.startswith("Bearer ") or not hmac.compare_digest(given, node["poll_secret_sha256"]):
            return self.answer(404)
        if not os.path.exists(os.path.join(self.server.state, "approved", node_id)):
            return self.answer(202)
        return self.answer(200, self.trust())

    def log_message(self, fmt, *args):
        print("registration: " + fmt % args, flush=True)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--listen", default="0.0.0.0:8443")
    p.add_argument("--fleet", required=True, help="the fleet's root.crt and bundle.json")
    p.add_argument("--cert", required=True, help="this endpoint's certificate and its issuing CA")
    p.add_argument("--key", required=True)
    p.add_argument("--state", required=True)
    a = p.parse_args()
    for d in ("nodes", "approved"):
        os.makedirs(os.path.join(a.state, d), exist_ok=True)

    host, port = a.listen.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), Registration)
    server.fleet, server.state = a.fleet, a.state
    server.lock = threading.Lock()
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(a.cert, a.key)
    server.socket = ctx.wrap_socket(server.socket, server_side=True)
    print(f"registration: listening on {a.listen}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
