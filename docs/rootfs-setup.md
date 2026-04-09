# Building the VM Root Filesystem

ExeClaw requires a root filesystem image (`rootfs.ext4`) with Ubuntu 24.04 and the nullclaw agent installed.

## Quick Overview

1. Create a 2GB ext4 image
2. Mount it and install Ubuntu 24.04 minimal via debootstrap
3. Install nullclaw and its dependencies inside the image
4. Configure auto-login on the serial console
5. Place the image at `vm-assets/rootfs.ext4`

## Steps

```bash
# Create a 2GB sparse file
dd if=/dev/zero of=rootfs.ext4 bs=1M count=2048
mkfs.ext4 rootfs.ext4

# Mount and populate
sudo mkdir -p /mnt/rootfs
sudo mount rootfs.ext4 /mnt/rootfs
sudo debootstrap --include=systemd,curl,ca-certificates noble /mnt/rootfs http://archive.ubuntu.com/ubuntu

# Chroot and install nullclaw
sudo chroot /mnt/rootfs /bin/bash
# ... install nullclaw, configure serial auto-login ...
exit

sudo umount /mnt/rootfs
mv rootfs.ext4 vm-assets/
```

## Kernel

You need an uncompressed Linux kernel (`vmlinux`). A kernel built with Firecracker's recommended config works best.

See [Firecracker's kernel docs](https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md) for details.
