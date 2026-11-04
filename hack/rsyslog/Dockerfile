# A real syslog server for hack/qemu-logforward-test.sh: Debian's
# rsyslog, receiving on UDP, TCP and TLS (gnutls) at the ports and with
# the certificates its configuration names (rsyslog.conf.in, filled in
# by the test), every message written as a JSON line to /out.
FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4
RUN apt-get update \
 && apt-get install -y --no-install-recommends rsyslog rsyslog-gnutls \
 && rm -rf /var/lib/apt/lists/*
CMD ["rsyslogd", "-n", "-f", "/etc/rsyslog-test/rsyslog.conf"]
