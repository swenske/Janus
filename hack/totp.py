#!/usr/bin/env python3
"""The TOTP code (RFC 6238: SHA-1, 30 s, 6 digits) of a base32 secret -
for test scripts that sign in to the Controller with a second factor.

usage: totp.py SECRET [STEPS]   STEPS: time steps from now (1: the next code)
"""
import base64, hashlib, hmac, struct, sys, time

secret = sys.argv[1].replace(" ", "").upper()
key = base64.b32decode(secret + "=" * (-len(secret) % 8))
step = int(time.time()) // 30 + (int(sys.argv[2]) if len(sys.argv) > 2 else 0)
h = hmac.new(key, struct.pack(">Q", step), hashlib.sha1).digest()
o = h[-1] & 15
print("%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000))
