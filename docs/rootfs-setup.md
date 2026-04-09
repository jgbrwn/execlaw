# Building the VM Root Filesystem & Kernel

ExeClaw requires two VM assets in `vm-assets/`:

- **`vmlinux`** — Uncompressed Linux kernel binary
- **`rootfs.ext4`** — 2GB ext4 filesystem image with Ubuntu 24.04 and nullclaw

These are not included in the repository due to size. Follow these exact steps to build them.

> **Note**: These are the exact commands used to build the rootfs that ships with ExeClaw.
> They were refined through several iterations of debugging outbound connectivity from
> inside Firecracker VMs (DNS resolution, CA certificates, kernel `ip=` networking).

## Prerequisites

```bash
sudo apt-get install -y wget curl e2fsprogs squashfs-tools
```

## Step 1: Download Firecracker Kernel and Base Ubuntu Rootfs

We use the official Firecracker CI kernel and Ubuntu 24.04 rootfs from Amazon S3.

```bash
mkdir -p vm-assets && cd vm-assets

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

# Download the Ubuntu 24.04 squashfs
latest_ubuntu_key=$(curl -s "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/$CI_VERSION/$ARCH/ubuntu-&list-type=2" \
    | grep -oP "(?<=<Key>)(firecracker-ci/$CI_VERSION/$ARCH/ubuntu-[0-9]+\.[0-9]+\.squashfs)(?=</Key>)" \
    | sort -V | tail -1)
wget -O ubuntu-24.04.squashfs "https://s3.amazonaws.com/spec.ccfc.min/$latest_ubuntu_key"
```

## Step 2: Extract Squashfs and Customize

Extract the base image, then install nullclaw and configure networking:

```bash
# Extract the squashfs image
sudo unsquashfs -d /tmp/squashfs-root ubuntu-24.04.squashfs
```

## Step 3: Install nullclaw

nullclaw is a statically linked binary — no runtime dependencies required.

```bash
# Download the latest nullclaw release
NCLAW_VERSION=$(curl -s https://api.github.com/repos/nullclaw/nullclaw/releases/latest \
    | grep -oP '"tag_name":\s*"\K[^"]+') 
wget -O /tmp/nullclaw \
    "https://github.com/nullclaw/nullclaw/releases/download/${NCLAW_VERSION}/nullclaw-linux-x86_64.bin"
chmod +x /tmp/nullclaw

# Install into the rootfs
sudo cp /tmp/nullclaw /tmp/squashfs-root/usr/local/bin/nullclaw
sudo chmod +x /tmp/squashfs-root/usr/local/bin/nullclaw
```

## Step 4: Install CA Certificates

**Critical**: The base Firecracker CI rootfs does NOT include CA certificates.
Without these, nullclaw cannot make HTTPS requests to any LLM API (OpenRouter,
Anthropic, OpenAI). This was the cause of `error.CurlFailed` errors during
initial development.

```bash
# Copy CA certificates from the host into the rootfs
sudo mkdir -p /tmp/squashfs-root/etc/ssl/certs
sudo cp /etc/ssl/certs/ca-certificates.crt /tmp/squashfs-root/etc/ssl/certs/
```

## Step 5: Configure Serial Console Auto-Login

ExeClaw communicates with VMs over the Firecracker serial console.
The VM must auto-login as root so the backend can immediately start nullclaw.

```bash
sudo mkdir -p /tmp/squashfs-root/etc/systemd/system/serial-getty@ttyS0.service.d
sudo tee /tmp/squashfs-root/etc/systemd/system/serial-getty@ttyS0.service.d/override.conf > /dev/null << 'EOF'
[Service]
# systemd requires this empty ExecStart line to override
ExecStart=
ExecStart=-/sbin/agetty --autologin root -o '-p -- \u' --keep-baud 115200,38400,9600 %I dumb
EOF
```

## Step 6: Configure DNS

ExeClaw uses the kernel `ip=` boot parameter for network interface setup
(this is faster and more reliable than userspace configuration). However,
the kernel `ip=` parameter does NOT configure DNS. We bake `resolv.conf`
directly into the rootfs.

**Background**: During development, we went through several iterations:
1. First tried `rc.local` with custom `fc_ip`/`fc_gw`/`fc_dns` kernel params — unreliable
2. Then tried the Firecracker CI `fcnet-setup.sh` — doesn't work because it expects MAC prefix `06:00:` but ExeClaw uses `02:FC:`
3. Final solution: kernel `ip=` for interface + static `resolv.conf` for DNS

```bash
# Set DNS resolvers
sudo tee /tmp/squashfs-root/etc/resolv.conf > /dev/null << 'EOF'
nameserver 8.8.8.8
nameserver 1.1.1.1
EOF
```

We also keep a minimal `rc.local` as a fallback networking mechanism and for setting the hostname:

```bash
sudo tee /tmp/squashfs-root/etc/rc.local > /dev/null << 'RCEOF'
#!/bin/bash
# Fallback: auto-configure network from kernel cmdline params
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
sudo chmod +x /tmp/squashfs-root/etc/rc.local

# Create systemd service for rc.local
sudo tee /tmp/squashfs-root/etc/systemd/system/rc-local.service > /dev/null << 'EOF'
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

# Enable rc-local service
sudo ln -sf /etc/systemd/system/rc-local.service \
    /tmp/squashfs-root/etc/systemd/system/multi-user.target.wants/rc-local.service
```

## Step 7: Build the ext4 Image

```bash
# Set ownership and create the 2GB ext4 image
cd vm-assets  # make sure you're in the vm-assets directory
sudo chown -R root:root /tmp/squashfs-root
truncate -s 2G rootfs.ext4
sudo mkfs.ext4 -d /tmp/squashfs-root -F rootfs.ext4

# Clean up
sudo rm -rf /tmp/squashfs-root
```

## Step 8: Verify

```bash
e2fsck -fn rootfs.ext4
echo "Kernel: $(file vmlinux | cut -d: -f2)"
echo "Rootfs: $(du -h rootfs.ext4 | cut -f1)"

# Quick verification that key files are present
debugfs -R 'stat /usr/local/bin/nullclaw' rootfs.ext4 2>/dev/null | head -3
debugfs -R 'stat /etc/ssl/certs/ca-certificates.crt' rootfs.ext4 2>/dev/null | head -3
debugfs -R 'cat /etc/resolv.conf' rootfs.ext4 2>/dev/null
```

## Final Layout

```
vm-assets/
├── vmlinux              # ~39MB, Linux 6.1.x kernel (from Firecracker CI)
├── rootfs.ext4          # 2GB ext4, Ubuntu 24.04 + nullclaw + CA certs
└── ubuntu-24.04.squashfs # (optional) keep for rebuilds
```

## How ExeClaw Uses the Rootfs at Runtime

ExeClaw does **not** modify the base rootfs. For each session it:

1. **Copy-on-write clone**: `cp --reflink=auto vm-assets/rootfs.ext4 /tmp/execlaw-SESSION.ext4`
2. **Inject config via debugfs**: Writes the nullclaw config (API key, model, provider) into `/root/.nullclaw/config.json` on the clone
3. **Boot Firecracker** with the cloned rootfs and kernel boot args:
   ```
   console=ttyS0 reboot=k panic=1 pci=off ip=GUEST_IP::GATEWAY:255.255.255.252:nullclaw:eth0:off
   ```
   The `ip=` parameter configures the network interface at kernel level (before systemd starts)
4. **Auto-login** — The serial console auto-logs in as root (via the agetty override)
5. **Start nullclaw** — The backend sends `nullclaw agent\n` over the serial console
6. **Cleanup** — On session end, the clone is deleted

## Troubleshooting

### `error.CurlFailed` from nullclaw
- **Missing CA certs**: Verify `/etc/ssl/certs/ca-certificates.crt` exists in the rootfs
- **No DNS**: Verify `/etc/resolv.conf` has nameservers in the rootfs
- **No network**: Check that the host has IP forwarding enabled (`sysctl net.ipv4.ip_forward=1`) and iptables MASQUERADE rule for the `10.200.0.0/16` range

### VM boots but no network
- Firecracker uses `virtio-mmio` (not PCI), so `pci=off` is correct
- The `virtio_net` driver must be built into the kernel (it is in the Firecracker CI kernel)
- Check that the TAP device is created and has an IP on the host side

### `fcnet-setup.sh` doesn't work
- This is expected. The Firecracker CI rootfs ships with `fcnet-setup.sh` which parses MAC addresses with prefix `06:00:`. ExeClaw uses MAC prefix `02:FC:` so this script is ignored. Networking is handled by the kernel `ip=` boot parameter instead.
