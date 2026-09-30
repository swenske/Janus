#!/usr/bin/env python3
"""A minimal QEMU guest agent client for the QEMU tests.

usage: qga-client.py SOCKET COMMAND [JSON-ARGUMENTS]

Talks to qemu-ga over the host side of its virtio-serial channel (a QEMU
-chardev socket): resyncs with guest-sync-delimited, sends one command,
prints the reply as JSON on one line. Exit 1 if the agent returns an
error (the error is printed), 2 if it doesn't answer.
"""
import json
import random
import socket
import sys

sock_path, command = sys.argv[1], sys.argv[2]
arguments = json.loads(sys.argv[3]) if len(sys.argv) > 3 else None

s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(10)
s.connect(sock_path)
f = s.makefile("rwb", buffering=0)


def send(msg):
    f.write(json.dumps(msg).encode() + b"\n")


def read_reply():
    line = b""
    while True:
        ch = f.read(1)
        if not ch:
            raise EOFError
        if ch == b"\xff":  # guest-sync-delimited's sentinel: discard stale bytes
            line = b""
            continue
        if ch == b"\n":
            if line.strip():
                return json.loads(line)
            line = b""
            continue
        line += ch


try:
    token = random.randint(1, 2**31)
    f.write(b"\xff")
    send({"execute": "guest-sync-delimited", "arguments": {"id": token}})
    while read_reply().get("return") != token:
        pass
    msg = {"execute": command}
    if arguments is not None:
        msg["arguments"] = arguments
    send(msg)
    if command == "guest-shutdown":
        # No reply on success: the guest goes down.
        print(json.dumps({"return": {}}))
        sys.exit(0)
    reply = read_reply()
except (OSError, EOFError, socket.timeout) as e:
    print(json.dumps({"error": {"class": "NoReply", "desc": str(e)}}))
    sys.exit(2)
print(json.dumps(reply))
sys.exit(1 if "error" in reply else 0)
