#!/bin/bash
# Starts the daemons, then the storage pool and the network the test's
# machines use, and waits.
set -euo pipefail

# QEMU runs with /dev/kvm's group: in an unprivileged LXC (the CI
# runners), the device's owner isn't mapped and root gets no right on it
# but the group's - libvirt then sees no KVM, and finds no UEFI firmware
# for a KVM domain. The container gets that group too (docker run
# --group-add), for libvirtd's own check.
echo "group = \"+$(stat -c %g /dev/kvm)\"" >> /etc/libvirt/qemu.conf

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
# A second network, for an interface added to a machine.
cat > /tmp/net2.xml <<'XML'
<network>
  <name>janus-test2</name>
  <bridge name='virbr-janus2' stp='on' delay='0'/>
  <ip address='192.168.124.1' netmask='255.255.255.0'/>
</network>
XML
virsh -q net-define /tmp/net2.xml
virsh -q net-start janus-test2
echo "libvirt-host ready"
exec sleep infinity
