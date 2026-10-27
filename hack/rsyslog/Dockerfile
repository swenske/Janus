# A real syslog server for hack/qemu-logforward-test.sh: Debian's
# rsyslog, receiving on UDP, TCP and TLS (gnutls) at the ports and with
# the certificates its configuration names (rsyslog.conf.in, filled in
# by the test), every message written as a JSON line to /out.
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
RUN apt-get update \
 && apt-get install -y --no-install-recommends rsyslog rsyslog-gnutls \
 && rm -rf /var/lib/apt/lists/*
CMD ["rsyslogd", "-n", "-f", "/etc/rsyslog-test/rsyslog.conf"]
