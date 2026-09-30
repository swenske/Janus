#!/usr/bin/env python3
"""A tiny peer on an 802.1Q VLAN, for hack/qemu-network-config-test.sh.

Listens for one QEMU `-netdev socket,connect=...` connection (Ethernet
frames, each prefixed by its 4-byte big-endian length) and, on VLAN
VID only, answers ARP for IP and NTP (SNTP server, stratum 1, the host's
clock) at IP. Frames that aren't tagged with VID are ignored, so an
answer proves the guest really sent them on that VLAN. Logs what it saw
and answered, one line per event, to LOG.

usage: vlan-ntp-responder.py PORT VID IP LOG
"""
import socket
import struct
import sys
import time

PORT, VID, IP, LOG = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
MAC = bytes.fromhex("525400aabbcc")
IP_BYTES = socket.inet_aton(IP)
NTP_EPOCH = 2208988800  # 1900-01-01 to 1970-01-01


def log(msg):
    with open(LOG, "a") as f:
        f.write(msg + "\n")


def ntp_ts(t):
    sec = int(t)
    return struct.pack("!II", sec + NTP_EPOCH, int((t - sec) * 2**32))


def ip_checksum(hdr):
    s = sum(struct.unpack("!10H", hdr))
    s = (s >> 16) + (s & 0xFFFF)
    s += s >> 16
    return (~s) & 0xFFFF


def tagged(dst, ethertype, payload):
    return dst + MAC + struct.pack("!HHH", 0x8100, VID, ethertype) + payload


def handle(frame):
    if len(frame) < 18 or struct.unpack("!H", frame[12:14])[0] != 0x8100:
        return None
    vid = struct.unpack("!H", frame[14:16])[0] & 0x0FFF
    if vid != VID:
        return None
    src_mac, ethertype, body = frame[6:12], struct.unpack("!H", frame[16:18])[0], frame[18:]

    if ethertype == 0x0806 and len(body) >= 28:  # ARP
        op = struct.unpack("!H", body[6:8])[0]
        sha, spa, tpa = body[8:14], body[14:18], body[24:28]
        if op == 1 and tpa == IP_BYTES:
            log(f"arp-request vid={vid} from {socket.inet_ntoa(spa)}")
            reply = struct.pack("!HHBBH", 1, 0x0800, 6, 4, 2) + MAC + IP_BYTES + sha + spa
            return tagged(src_mac, 0x0806, reply)

    if ethertype == 0x0800 and len(body) >= 28 and body[9] == 17:  # IPv4/UDP
        ihl = (body[0] & 0x0F) * 4
        src_ip, dst_ip = body[12:16], body[16:20]
        udp = body[ihl:]
        sport, dport = struct.unpack("!HH", udp[0:4])
        req = udp[8:]
        if dst_ip != IP_BYTES or dport != 123 or len(req) < 48:
            return None
        now = time.time()
        log(f"ntp-request vid={vid} from {socket.inet_ntoa(src_ip)}")
        # LI 0, version 4, mode 4 (server); stratum 1; the client's poll;
        # precision 2^-20; root delay 0, dispersion ~1ms; "LOCL".
        ntp = struct.pack("!BBbbII", 0x24, 1, req[2], -20, 0, 0x00000042) + b"LOCL"
        ntp += ntp_ts(now - 1) + req[40:48] + ntp_ts(now) + ntp_ts(time.time())
        udp_out = struct.pack("!HHHH", 123, sport, 8 + len(ntp), 0) + ntp  # checksum 0: none
        hdr = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(udp_out), 0, 0, 64, 17, 0, IP_BYTES, src_ip)
        hdr = hdr[:10] + struct.pack("!H", ip_checksum(hdr)) + hdr[12:]
        return tagged(src_mac, 0x0800, hdr + udp_out)
    return None


def recv_exact(conn, n):
    buf = b""
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            raise EOFError
        buf += chunk
    return buf


srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", PORT))
srv.listen(1)
log("listening")
while True:  # QEMU reconnects after a guest reboot only if restarted; keep serving
    conn, _ = srv.accept()
    log("connected")
    try:
        while True:
            (length,) = struct.unpack("!I", recv_exact(conn, 4))
            reply = handle(recv_exact(conn, length))
            if reply is not None:
                reply = reply.ljust(60, b"\0")
                conn.sendall(struct.pack("!I", len(reply)) + reply)
    except (EOFError, ConnectionResetError):
        log("disconnected")
        conn.close()
