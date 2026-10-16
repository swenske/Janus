# A real syslog server for hack/qemu-logforward-test.sh: Debian's
# rsyslog, receiving on UDP, TCP and TLS (gnutls) at the ports and with
# the certificates its configuration names (rsyslog.conf.in, filled in
# by the test), every message written as a JSON line to /out.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
RUN apt-get update \
 && apt-get install -y --no-install-recommends rsyslog rsyslog-gnutls \
 && rm -rf /var/lib/apt/lists/*
CMD ["rsyslogd", "-n", "-f", "/etc/rsyslog-test/rsyslog.conf"]
