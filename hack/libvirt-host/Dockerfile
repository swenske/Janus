# A libvirt/KVM host in a container, for hack/controller-libvirt-test.sh:
# libvirtd, QEMU with OVMF, and sshd - the Controller reaches libvirt
# the way it reaches a real hypervisor (SSH as a dedicated user in the
# libvirt group, its socket forwarded). Not a target artifact.
FROM debian:13-slim

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      libvirt-daemon-system libvirt-clients qemu-system-x86 qemu-utils ovmf \
      openssh-server dnsmasq-base iptables nftables iproute2 python3 ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# No cgroups or security driver of its own inside a container; QEMU as
# root so it can open the pool's volumes as they are (its group is
# /dev/kvm's, set by entrypoint.sh) - so no ownership changes either,
# nor remembering them in trusted.* xattrs, which an unprivileged LXC
# (the CI runners) refuses. No polkit either (no D-Bus): the socket's
# access is its group's, libvirt.
RUN printf 'cgroup_controllers = [ ]\nsecurity_driver = "none"\nuser = "root"\ndynamic_ownership = 0\nremember_owner = 0\n' >> /etc/libvirt/qemu.conf \
    && printf 'unix_sock_group = "libvirt"\nunix_sock_rw_perms = "0770"\nauth_unix_rw = "none"\nauth_unix_ro = "none"\n' >> /etc/libvirt/libvirtd.conf

# The Controller's user: SSH key only, libvirt through the group.
RUN useradd -m -s /bin/bash -G libvirt janus-ctl && passwd -l janus-ctl \
    && mkdir -p /home/janus-ctl/.ssh /run/sshd && chmod 700 /home/janus-ctl/.ssh \
    && chown -R janus-ctl: /home/janus-ctl/.ssh \
    && sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config

COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
