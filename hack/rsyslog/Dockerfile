# A real syslog server for hack/qemu-logforward-test.sh: Debian's
# rsyslog, receiving on UDP, TCP and TLS (gnutls) at the ports and with
# the certificates its configuration names (rsyslog.conf.in, filled in
# by the test), every message written as a JSON line to /out.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
RUN apt-get update \
 && apt-get install -y --no-install-recommends rsyslog rsyslog-gnutls \
 && rm -rf /var/lib/apt/lists/*
CMD ["rsyslogd", "-n", "-f", "/etc/rsyslog-test/rsyslog.conf"]
