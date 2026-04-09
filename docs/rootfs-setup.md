# Building the VM Root Filesystem & Kernel

ExeClaw requires two VM assets in `vm-assets/`:

- **`vmlinux`** — Uncompressed Linux kernel binary
- **`rootfs.ext4`** — 2GB ext4 filesystem image with Ubuntu 24.04 and nullclaw

These are not included in the repository due to size. Follow these exact steps to build them.

## Prerequisites

```bash
sudo apt-get install -y wget curl e2fsprogs squashfs-tools
```

## Step 1: Download Firecracker Kernel and Base Rootfs

We use the official Firecracker CI kernel and Ubuntu rootfs as the base.

```bash
mkdir -p vm-assets && cd vm-assets

# Determine latest Firecracker CI version
ARCH="$(uname -m)"
release_url="https://github.com/firecracker-microvm/firecracker/releases"
latest_version=$(basename $(curl -fsSLI -o /dev/null -w %{url_effective} ${release_url}/latest))
CI_VERSION=${latest_version%.*}

# Download the kernel
latest_kernel_key=$(curl -s "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/$CI_VERSION/$ARCH/vmlinux-&list-type=2" \
    | grep -oP "(?<=<Key>)(firecracker-ci/$CI_VERSION/$ARCH/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" \
    | sort -V | tail -1)
wget -O vmlinux "https://s3.amazonaws.com/spec.ccfc.min/${latest_kernel_key}"
chmod +x vmlinux

# Download the Ubuntu squashfs
latest_ubuntu_key=$(curl -s "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/$CI_VERSION/$ARCH/ubuntu-&list-type=2" \
    | grep -oP "(?<=<Key>)(firecracker-ci/$CI_VERSION/$ARCH/ubuntu-[0-9]+\.[0-9]+\.squashfs)(?=</Key>)" \
    | sort -V | tail -1)
wget -O ubuntu-base.squashfs "https://s3.amazonaws.com/spec.ccfc.min/$latest_ubuntu_key"
```

## Step 2: Convert Squashfs to ext4

```bash
# Extract the squashfs
sudo unsquashfs ubuntu-base.squashfs

# Create a 2GB ext4 image from the extracted contents
sudo chown -R root:root squashfs-root
truncate -s 2G rootfs.ext4
sudo mkfs.ext4 -d squashfs-root -F rootfs.ext4

# Clean up
sudo rm -rf squashfs-root ubuntu-base.squashfs
```

## Step 3: Install nullclaw into the Rootfs

Mount the rootfs and install the nullclaw binary:

```bash
sudo mkdir -p /mnt/rootfs
sudo mount rootfs.ext4 /mnt/rootfs

# Download nullclaw (statically linked, no dependencies)
NCLAW_VERSION=$(curl -s https://api.github.com/repos/nullclaw/nullclaw/releases/latest | grep -oP '"tag_name":\s*"\K[^"]+') 
wget -O /tmp/nullclaw "https://github.com/nullclaw/nullclaw/releases/download/${NCLAW_VERSION}/nullclaw-linux-x86_64.bin"
chmod +x /tmp/nullclaw
sudo cp /tmp/nullclaw /mnt/rootfs/usr/local/bin/nullclaw
```

## Step 4: Configure Auto-Login on Serial Console

The VM boots to a serial console. We need root auto-login so ExeClaw can
send commands immediately after boot.

```bash
# Create the getty override for auto-login on ttyS0
sudo mkdir -p /mnt/rootfs/etc/systemd/system/serial-getty@ttyS0.service.d
sudo tee /mnt/rootfs/etc/systemd/system/serial-getty@ttyS0.service.d/override.conf > /dev/null << 'EOF'
[Service]
# systemd requires this empty ExecStart line to override
ExecStart=
ExecStart=-/sbin/agetty --autologin root -o '-p -- \u' --keep-baud 115200,38400,9600 %I dumb
EOF
```

## Step 5: Configure Networking

ExeClaw uses the kernel `ip=` boot parameter for primary network setup, but
we also install an rc.local fallback that reads custom `fc_ip`/`fc_gw`/`fc_dns`
kernel cmdline parameters:

```bash
# Create rc.local for network configuration
sudo tee /mnt/rootfs/etc/rc.local > /dev/null << 'RCEOF'
#!/bin/bash
# Auto-configure network from kernel cmdline params
FC_IP=$(cat /proc/cmdline | grep -oP "fc_ip=\K[^ ]+")
FC_GW=$(cat /proc/cmdline | grep -oP "fc_gw=\K[^ ]+")
FC_DNS=$(cat /proc/cmdline | grep -oP "fc_dns=\K[^ ]+")

if [ -n "$FC_IP" ] && [ -n "$FC_GW" ]; then
    ip addr add "$FC_IP/30" dev eth0
    ip link set eth0 up
    ip route add default via "$FC_GW"
    if [ -n "$FC_DNS" ]; then
        echo "nameserver $FC_DNS" > /etc/resolv.conf
    else
        echo "nameserver 8.8.8.8" > /etc/resolv.conf
        echo "nameserver 1.1.1.1" >> /etc/resolv.conf
    fi
fi
hostname nullclaw-sandbox
RCEOF
sudo chmod +x /mnt/rootfs/etc/rc.local

# Create systemd service for rc.local
sudo tee /mnt/rootfs/etc/systemd/system/rc-local.service > /dev/null << 'EOF'
[Unit]
Description=rc.local
After=network-pre.target

[Service]
Type=oneshot
ExecStart=/etc/rc.local
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF

# Enable the rc-local service
sudo ln -sf /etc/systemd/system/rc-local.service \
    /mnt/rootfs/etc/systemd/system/multi-user.target.wants/rc-local.service
```

## Step 6: Set DNS defaults

```bash
sudo tee /mnt/rootfs/etc/resolv.conf > /dev/null << 'EOF'
nameserver 8.8.8.8
nameserver 1.1.1.1
EOF
```

## Step 7: Unmount and Verify

```bash
sudo umount /mnt/rootfs

# Verify the image is valid
e2fsck -fn rootfs.ext4
echo "Kernel: $(file vmlinux | cut -d: -f2)"
echo "Rootfs: $(du -h rootfs.ext4 | cut -f1)"
```

## Final Layout

```
vm-assets/
├── vmlinux       # ~39MB, Linux 6.1.x kernel
└── rootfs.ext4   # 2GB ext4, Ubuntu 24.04 + nullclaw
```

## How ExeClaw Uses the Rootfs

At runtime, ExeClaw does **not** modify the base rootfs. For each session it:

1. Creates a copy-on-write clone: `cp --reflink=auto vm-assets/rootfs.ext4 /tmp/execlaw-SESSION.ext4`
2. Uses `debugfs` to write the nullclaw config (API key, model, provider) into `/root/.nullclaw/config.json` on the clone
3. Boots Firecracker with the cloned rootfs
4. After boot, sends `root` login via serial console, then runs `nullclaw agent`
5. On session cleanup, deletes the clone

This means the base rootfs is never modified and can be shared across all sessions.

## Kernel Boot Parameters

ExeClaw passes these boot args to the VM:

```
console=ttyS0 reboot=k panic=1 pci=off ip=GUEST_IP::GATEWAY:255.255.255.252:nullclaw:eth0:off
```

The `ip=` parameter configures networking at the kernel level before userspace starts, which is faster and more reliable than the rc.local fallback.
