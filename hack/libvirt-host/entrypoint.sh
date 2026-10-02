#!/bin/bash
# Starts the daemons, then the storage pool and the network the test's
# machines use, and waits.
set -euo pipefail

ssh-keygen -A >/dev/null
/usr/sbin/sshd
virtlogd -d
libvirtd -d
for _ in $(seq 1 50); do
  [ -S /var/run/libvirt/libvirt-sock ] && break
  sleep 0.2
done

mkdir -p /var/lib/libvirt/janus
virsh -q pool-define-as janus dir --target /var/lib/libvirt/janus
virsh -q pool-start janus
cat > /tmp/net.xml <<'XML'
<network>
  <name>janus-test</name>
  <forward mode='nat'/>
  <bridge name='virbr-janus' stp='on' delay='0'/>
  <ip address='192.168.123.1' netmask='255.255.255.0'>
    <dhcp><range start='192.168.123.100' end='192.168.123.199'/></dhcp>
  </ip>
</network>
XML
virsh -q net-define /tmp/net.xml
virsh -q net-start janus-test
echo "libvirt-host ready"
exec sleep infinity
