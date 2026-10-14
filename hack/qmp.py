#!/usr/bin/env python3
"""Sends one QMP command to a QEMU monitor socket and prints its result.

Usage: hack/qmp.py SOCKET EXECUTE [JSON-ARGUMENTS]

Prints the "return" value as JSON; exits 1 with QMP's error on stderr
when the command fails. Standard library only (the publish runner has
python3, nothing else).
"""
import json
import socket
import sys


def main():
    if len(sys.argv) not in (3, 4):
        sys.exit(__doc__)
    sock = socket.socket(socket.AF_UNIX)
    sock.settimeout(30)
    sock.connect(sys.argv[1])
    f = sock.makefile("rw")
    f.readline()  # the greeting

    def call(execute, arguments=None):
        msg = {"execute": execute}
        if arguments is not None:
            msg["arguments"] = arguments
        f.write(json.dumps(msg) + "\n")
        f.flush()
        while True:
            reply = json.loads(f.readline())
            if "event" not in reply:
                return reply

    call("qmp_capabilities")
    args = json.loads(sys.argv[3]) if len(sys.argv) == 4 else None
    reply = call(sys.argv[2], args)
    if "error" in reply:
        print(json.dumps(reply["error"]), file=sys.stderr)
        sys.exit(1)
    print(json.dumps(reply.get("return")))


if __name__ == "__main__":
    main()
