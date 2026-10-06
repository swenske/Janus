#!/usr/bin/env bash
# Signs a certificate request with a CA, valid from five minutes ago - a
# node whose clock is a little behind still takes it. openssl ca rather
# than openssl x509 -req: it sets the start date since OpenSSL 1.1.1.
#
#   sign.sh REQUEST.csr CA.crt CA.key EXTENSIONS-FILE OUT.crt HOURS
#
# EXTENSIONS-FILE: the certificate's extensions, one per line, in
# openssl's configuration syntax.
set -euo pipefail
csr=$1 ca=$2 cakey=$3 ext=$4 out=$5 hours=$6
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: > "$work/index"
cat > "$work/ca.cnf" <<CNF
[ca]
default_ca = sign
[sign]
database = $work/index
new_certs_dir = $work
rand_serial = yes
default_md = sha256
unique_subject = no
policy = subject
x509_extensions = extensions
[subject]
commonName = supplied
organizationName = optional
[extensions]
$(cat "$ext")
CNF
if ! openssl ca -batch -notext -preserveDN -config "$work/ca.cnf" -cert "$ca" -keyfile "$cakey" \
  -startdate "$(date -u -d '-5 minutes' +%Y%m%d%H%M%SZ)" \
  -enddate "$(date -u -d "+$hours hours" +%Y%m%d%H%M%SZ)" \
  -in "$csr" -out "$out" 2>"$work/log"; then
  cat "$work/log" >&2
  exit 1
fi
