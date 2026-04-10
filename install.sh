#!/bin/bash
set -e

###############################################################################
# ExeClaw Installation Script
# 
# This script automates the complete setup of ExeClaw, including:
# - Firecracker binary
# - VM kernel and rootfs with nullclaw
# - Network configuration (IP forwarding, iptables)
# - Systemd service
#
# Requirements: Ubuntu 22.04+ / Debian 12+, KVM support, Go 1.21+
###############################################################################

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Global variables
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMP_DIR=""
SUDO_CMD=""

###############################################################################
# Helper Functions
###############################################################################

print_banner() {
    echo -e "${BLUE}" 
    echo "═══════════════════════════════════════════════════════════════════════════"
    echo "  $1"
    echo "═══════════════════════════════════════════════════════════════════════════"
    echo -e "${NC}"
}

print_success() {
    echo -e "${GREEN}✓${NC} $1"
}

print_error() {
    echo -e "${RED}✗ Error:${NC} $1" >&2
}

print_warning() {
    echo -e "${YELLOW}⚠${NC} $1"
}

print_info() {
    echo -e "${BLUE}ℹ${NC} $1"
}

cleanup() {
    if [ -n "$TEMP_DIR" ] && [ -d "$TEMP_DIR" ]; then
        print_info "Cleaning up temporary files..."
        $SUDO_CMD rm -rf "$TEMP_DIR"
    fi
}

trap cleanup EXIT

check_command() {
    if command -v "$1" >/dev/null 2>&1; then
        return 0
    else
        return 1
    fi
}

get_latest_github_release() {
    local repo="$1"
    curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" \
        | grep -oP '"tag_name":\s*"\K[^"]+' || echo ""
}

###############################################################################
# Prerequisite Checks
###############################################################################

check_prerequisites() {
    print_banner "Checking Prerequisites"
    
    # Check if running as root or can sudo
    if [ "$EUID" -eq 0 ]; then
        SUDO_CMD=""
        print_success "Running as root"
    elif check_command sudo; then
        SUDO_CMD="sudo"
        print_success "sudo available"
        # Test sudo access
        if ! $SUDO_CMD -v; then
            print_error "sudo access required but not granted"
            exit 1
        fi
    else
        print_error "This script must be run as root or with sudo access"
        exit 1
    fi
    
    # Check KVM access
    if [ -c "/dev/kvm" ]; then
        print_success "KVM device found at /dev/kvm"
        if [ -r "/dev/kvm" ] && [ -w "/dev/kvm" ]; then
            print_success "KVM device is accessible"
        else
            print_warning "KVM device exists but may not be accessible to current user"
            print_info "You may need to add your user to the 'kvm' group: sudo usermod -aG kvm \$USER"
        fi
    else
        print_error "/dev/kvm not found. KVM support is required for Firecracker."
        print_info "Enable virtualization in BIOS and ensure KVM kernel modules are loaded."
        exit 1
    fi
    
    # Check Go installation
    if check_command go; then
        GO_VERSION=$(go version | grep -oP 'go[0-9]+\.[0-9]+' | sed 's/go//')
        GO_MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
        GO_MINOR=$(echo "$GO_VERSION" | cut -d. -f2)
        
        if [ "$GO_MAJOR" -gt 1 ] || ([ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -ge 21 ]); then
            print_success "Go $GO_VERSION installed (1.21+ required)"
        else
            print_error "Go $GO_VERSION found, but 1.21+ is required"
            exit 1
        fi
    else
        print_error "Go is not installed. Please install Go 1.21 or later."
        print_info "Visit: https://go.dev/doc/install"
        exit 1
    fi
    
    # Check for required system packages
    print_info "Checking for required system packages..."
    MISSING_PACKAGES=()
    for pkg in wget curl e2fsprogs squashfs-tools iptables; do
        if ! check_command "$pkg" && ! dpkg -l | grep -q "^ii  $pkg"; then
            MISSING_PACKAGES+=("$pkg")
        fi
    done
    
    if [ ${#MISSING_PACKAGES[@]} -gt 0 ]; then
        print_warning "Missing packages: ${MISSING_PACKAGES[*]}"
        print_info "Installing missing packages..."
        $SUDO_CMD apt-get update -qq
        $SUDO_CMD apt-get install -y "${MISSING_PACKAGES[@]}"
        print_success "Packages installed"
    else
        print_success "All required packages are installed"
    fi
    
    echo
}

###############################################################################
# Build Go Binary
###############################################################################

build_go_binary() {
    print_banner "Building ExeClaw Binary"
    
    if [ ! -f "$SCRIPT_DIR/main.go" ]; then
        print_warning "main.go not found, skipping Go build"
        echo
        return
    fi
    
    cd "$SCRIPT_DIR"
    
    if [ -f "$SCRIPT_DIR/execlaw" ]; then
        print_info "Existing execlaw binary found"
        read -p "Rebuild? [y/N] " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            print_info "Skipping build"
            echo
            return
        fi
    fi
    
    print_info "Building execlaw binary..."
    if go build -o execlaw .; then
        print_success "execlaw binary built successfully"
        chmod +x execlaw
    else
        print_error "Failed to build execlaw binary"
        exit 1
    fi
    
    echo
}

###############################################################################
# Download Firecracker
###############################################################################

download_firecracker() {
    print_banner "Installing Firecracker"
    
    if [ -f "/usr/local/bin/firecracker" ]; then
        FC_VERSION=$(/usr/local/bin/firecracker --version 2>&1 | head -n1 || echo "unknown")
        print_success "Firecracker already installed: $FC_VERSION"
        echo
        return
    fi
    
    print_info "Downloading Firecracker..."
    
    ARCH="$(uname -m)"
    LATEST_FC_VERSION=$(get_latest_github_release "firecracker-microvm/firecracker")
    
    if [ -z "$LATEST_FC_VERSION" ]; then
        print_error "Failed to fetch latest Firecracker version"
        exit 1
    fi
    
    print_info "Latest Firecracker version: $LATEST_FC_VERSION"
    
    FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${LATEST_FC_VERSION}/firecracker-${LATEST_FC_VERSION}-${ARCH}.tgz"
    
    TEMP_DIR=$(mktemp -d)
    cd "$TEMP_DIR"
    
    if wget -q "$FC_URL"; then
        tar -xzf "firecracker-${LATEST_FC_VERSION}-${ARCH}.tgz"
        $SUDO_CMD cp "release-${LATEST_FC_VERSION}-${ARCH}/firecracker-${LATEST_FC_VERSION}-${ARCH}" /usr/local/bin/firecracker
        $SUDO_CMD chmod +x /usr/local/bin/firecracker
        print_success "Firecracker installed to /usr/local/bin/firecracker"
    else
        print_error "Failed to download Firecracker from $FC_URL"
        exit 1
    fi
    
    cd "$SCRIPT_DIR"
    echo
}

###############################################################################
# Download VM Kernel
###############################################################################

download_vm_kernel() {
    print_banner "Downloading VM Kernel"
    
    mkdir -p "$SCRIPT_DIR/vm-assets"
    
    if [ -f "$SCRIPT_DIR/vm-assets/vmlinux" ]; then
        KERNEL_SIZE=$(du -h "$SCRIPT_DIR/vm-assets/vmlinux" | cut -f1)
        print_success "VM kernel already exists (${KERNEL_SIZE})"
        echo
        return
    fi
    
    print_info "Fetching kernel from Firecracker CI S3..."
    
    ARCH="$(uname -m)"
    LATEST_FC_VERSION=$(get_latest_github_release "firecracker-microvm/firecracker")
    CI_VERSION="${LATEST_FC_VERSION%.*}"
    
    if [ -z "$LATEST_FC_VERSION" ] || [ -z "$CI_VERSION" ]; then
        print_error "Failed to determine Firecracker CI version"
        exit 1
    fi
    
    print_info "Using Firecracker CI version: $CI_VERSION"
    
    # Query S3 for latest kernel
    S3_LIST_URL="https://s3.amazonaws.com/spec.ccfc.min/?prefix=firecracker-ci/$CI_VERSION/$ARCH/vmlinux-&list-type=2"
    KERNEL_KEY=$(curl -fsSL "$S3_LIST_URL" \
        | grep -oP "(?<=<Key>)(firecracker-ci/$CI_VERSION/$ARCH/vmlinux-[0-9]+(?:\.[0-9]+)+)(?=</Key>)" \
        | sort -V | tail -1)
    
    if [ -z "$KERNEL_KEY" ]; then
        print_error "Failed to find kernel in S3"
        print_info "Tried prefix: firecracker-ci/$CI_VERSION/$ARCH/vmlinux-"
        exit 1
    fi
    
    print_info "Downloading kernel: $KERNEL_KEY"
    
    KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/${KERNEL_KEY}"
    
    if wget -q --show-progress -O "$SCRIPT_DIR/vm-assets/vmlinux" "$KERNEL_URL"; then
        chmod +x "$SCRIPT_DIR/vm-assets/vmlinux"
        KERNEL_SIZE=$(du -h "$SCRIPT_DIR/vm-assets/vmlinux" | cut -f1)
        print_success "Kernel downloaded successfully (${KERNEL_SIZE})"
    else
        print_error "Failed to download kernel from $KERNEL_URL"
        exit 1
    fi
    
    echo
}

###############################################################################
# Build VM Rootfs
###############################################################################

build_vm_rootfs() {
    print_banner "Building VM Rootfs"
    
    if [ -f "$SCRIPT_DIR/vm-assets/rootfs.ext4" ]; then
        ROOTFS_SIZE=$(du -h "$SCRIPT_DIR/vm-assets/rootfs.ext4" | cut -f1)
        print_success "VM rootfs already exists (${ROOTFS_SIZE})"
        echo
        return
    fi
    
    print_info "This will build a 2GB Ubuntu 24.04 rootfs with nullclaw..."
    
    mkdir -p "$SCRIPT_DIR/vm-assets"
    cd "$SCRIPT_DIR/vm-assets"
    
    ARCH="$(uname -m)"
    LATEST_FC_VERSION=$(get_latest_github_release "firecracker-microvm/firecracker")
    CI_VERSION="${LATEST_FC_VERSION%.*}"
    
    if [ -z "$LATEST_FC_VERSION" ] || [ -z "$CI_VERSION" ]; then
        print_error "Failed to determine Firecracker CI version"
        exit 1
    fi
    
    # Download Ubuntu squashfs if not present
    if [ ! -f "ubuntu-24.04.squashfs" ]; then
        print_info "Downloading Ubuntu 24.04 base image from Firecracker CI..."
        
        S3_LIST_URL="https://s3.amazonaws.com/spec.ccfc.min/?prefix=firecracker-ci/$CI_VERSION/$ARCH/ubuntu-&list-type=2"
        UBUNTU_KEY=$(curl -fsSL "$S3_LIST_URL" \
            | grep -oP "(?<=<Key>)(firecracker-ci/$CI_VERSION/$ARCH/ubuntu-[0-9]+(?:\.[0-9]+)+\.squashfs)(?=</Key>)" \
            | sort -V | tail -1)
        
        if [ -z "$UBUNTU_KEY" ]; then
            print_error "Failed to find Ubuntu squashfs in S3"
            print_info "Tried prefix: firecracker-ci/$CI_VERSION/$ARCH/ubuntu-"
            exit 1
        fi
        
        print_info "Downloading: $UBUNTU_KEY"
        UBUNTU_URL="https://s3.amazonaws.com/spec.ccfc.min/$UBUNTU_KEY"
        
        if ! wget -q --show-progress -O ubuntu-24.04.squashfs "$UBUNTU_URL"; then
            print_error "Failed to download Ubuntu squashfs"
            exit 1
        fi
        print_success "Ubuntu base image downloaded"
    else
        print_success "Ubuntu squashfs already downloaded"
    fi
    
    # Extract squashfs
    print_info "Extracting squashfs (this may take a minute)..."
    TEMP_DIR=$(mktemp -d)
    
    if ! $SUDO_CMD unsquashfs -d "$TEMP_DIR/squashfs-root" ubuntu-24.04.squashfs >/dev/null 2>&1; then
        print_error "Failed to extract squashfs"
        exit 1
    fi
    print_success "Squashfs extracted"
    
    # Download and install nullclaw
    print_info "Downloading nullclaw..."
    NCLAW_VERSION=$(get_latest_github_release "nullclaw/nullclaw")
    
    if [ -z "$NCLAW_VERSION" ]; then
        print_error "Failed to fetch latest nullclaw version"
        exit 1
    fi
    
    print_info "Latest nullclaw version: $NCLAW_VERSION"

    case "$ARCH" in
        x86_64) NCLAW_ARCH="x86_64" ;;
        aarch64|arm64) NCLAW_ARCH="aarch64" ;;
        *)
            print_error "Unsupported architecture for nullclaw: $ARCH"
            exit 1
	    ;;
    esac

    NCLAW_URL="https://github.com/nullclaw/nullclaw/releases/download/${NCLAW_VERSION}/nullclaw-linux-${NCLAW_ARCH}.bin"
    
    if wget -q -O /tmp/nullclaw "$NCLAW_URL"; then
        chmod +x /tmp/nullclaw
        $SUDO_CMD cp /tmp/nullclaw "$TEMP_DIR/squashfs-root/usr/local/bin/nullclaw"
        $SUDO_CMD chmod +x "$TEMP_DIR/squashfs-root/usr/local/bin/nullclaw"
        rm /tmp/nullclaw
        print_success "nullclaw installed into rootfs"
    else
        print_error "Failed to download nullclaw"
        exit 1
    fi
    
    # Install CA certificates
    print_info "Installing CA certificates..."
    $SUDO_CMD mkdir -p "$TEMP_DIR/squashfs-root/etc/ssl/certs"
    if [ -f "/etc/ssl/certs/ca-certificates.crt" ]; then
        $SUDO_CMD cp /etc/ssl/certs/ca-certificates.crt "$TEMP_DIR/squashfs-root/etc/ssl/certs/"
        print_success "CA certificates copied"
    else
        print_warning "Host CA certificates not found, HTTPS may not work in VM"
    fi
    
    # Configure serial console auto-login
    print_info "Configuring serial console auto-login..."
    $SUDO_CMD mkdir -p "$TEMP_DIR/squashfs-root/etc/systemd/system/serial-getty@ttyS0.service.d"
    $SUDO_CMD tee "$TEMP_DIR/squashfs-root/etc/systemd/system/serial-getty@ttyS0.service.d/override.conf" > /dev/null << 'EOF'
[Service]
# systemd requires this empty ExecStart line to override
ExecStart=
ExecStart=-/sbin/agetty --autologin root -o '-p -- \u' --keep-baud 115200,38400,9600 %I dumb
EOF
    print_success "Serial console auto-login configured"
    
    # Configure DNS
    print_info "Configuring DNS..."
    $SUDO_CMD tee "$TEMP_DIR/squashfs-root/etc/resolv.conf" > /dev/null << 'EOF'
nameserver 8.8.8.8
nameserver 1.1.1.1
EOF
    print_success "DNS configured"
    
    # Create rc.local
    print_info "Creating rc.local fallback networking script..."
    $SUDO_CMD tee "$TEMP_DIR/squashfs-root/etc/rc.local" > /dev/null << 'RCEOF'
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
    $SUDO_CMD chmod +x "$TEMP_DIR/squashfs-root/etc/rc.local"
    print_success "rc.local created"
    
    # Create rc-local.service
    print_info "Creating rc-local.service..."
    $SUDO_CMD tee "$TEMP_DIR/squashfs-root/etc/systemd/system/rc-local.service" > /dev/null << 'EOF'
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
    
    $SUDO_CMD ln -sf /etc/systemd/system/rc-local.service \
        "$TEMP_DIR/squashfs-root/etc/systemd/system/multi-user.target.wants/rc-local.service"
    print_success "rc-local.service created and enabled"
    
    # Build ext4 image
    print_info "Building 2GB ext4 image (this may take a few minutes)..."
    $SUDO_CMD chown -R root:root "$TEMP_DIR/squashfs-root"
    truncate -s 2G rootfs.ext4
    
    if $SUDO_CMD mkfs.ext4 -d "$TEMP_DIR/squashfs-root" -F rootfs.ext4 >/dev/null 2>&1; then
        print_success "ext4 rootfs image created successfully"
    else
        print_error "Failed to create ext4 image"
        exit 1
    fi
    
    # Cleanup temp files
    print_info "Cleaning up temporary files..."
    $SUDO_CMD rm -rf "$TEMP_DIR/squashfs-root"
    
    # Verify
    print_info "Verifying rootfs..."
    if e2fsck -fn rootfs.ext4 >/dev/null 2>&1; then
        print_success "Rootfs verification passed"
    else
        print_warning "Rootfs verification had warnings (usually safe to ignore)"
    fi
    
    ROOTFS_SIZE=$(du -h rootfs.ext4 | cut -f1)
    print_success "Rootfs built successfully (${ROOTFS_SIZE})"
    
    cd "$SCRIPT_DIR"
    echo
}

###############################################################################
# Configure Network
###############################################################################

configure_network() {
    print_banner "Configuring Network"
    
    # Enable IP forwarding
    print_info "Enabling IP forwarding..."
    CURRENT_FORWARD=$($SUDO_CMD sysctl -n net.ipv4.ip_forward)
    
    if [ "$CURRENT_FORWARD" = "1" ]; then
        print_success "IP forwarding already enabled"
    else
        $SUDO_CMD sysctl -w net.ipv4.ip_forward=1 >/dev/null
        print_success "IP forwarding enabled"
        
        # Make it persistent
        if ! grep -q "^net.ipv4.ip_forward=1" /etc/sysctl.conf 2>/dev/null; then
            print_info "Making IP forwarding persistent..."
            echo "net.ipv4.ip_forward=1" | $SUDO_CMD tee -a /etc/sysctl.conf >/dev/null
            print_success "IP forwarding will persist across reboots"
        fi
    fi
    
    # Configure iptables NAT
    print_info "Configuring iptables NAT for 10.200.0.0/16..."
    
    # Check if rule already exists
    if $SUDO_CMD iptables -t nat -C POSTROUTING -s 10.200.0.0/16 -j MASQUERADE 2>/dev/null; then
        print_success "iptables MASQUERADE rule already exists"
    else
        $SUDO_CMD iptables -t nat -A POSTROUTING -s 10.200.0.0/16 -j MASQUERADE
        print_success "iptables MASQUERADE rule added"
        
        # Try to persist iptables rules
        if check_command iptables-save && check_command netfilter-persistent; then
            print_info "Saving iptables rules..."
            $SUDO_CMD netfilter-persistent save >/dev/null 2>&1 || true
            print_success "iptables rules saved"
        elif check_command iptables-save; then
            print_warning "netfilter-persistent not found, iptables rules may not persist"
            print_info "Install iptables-persistent to make rules permanent:"
            print_info "  sudo apt-get install iptables-persistent"
        fi
    fi
    
    echo
}

###############################################################################
# Install Systemd Service
###############################################################################

install_systemd_service() {
    print_banner "Installing Systemd Service"
    
    if [ ! -f "$SCRIPT_DIR/execlaw.service" ]; then
        print_warning "execlaw.service file not found, skipping systemd setup"
        echo
        return
    fi
    
    # Get the current user and home directory
    if [ "$EUID" -eq 0 ]; then
        # Running as root, need to determine the actual user
        if [ -n "$SUDO_USER" ]; then
            INSTALL_USER="$SUDO_USER"
            INSTALL_HOME=$(eval echo ~"$SUDO_USER")
        else
            print_warning "Running as root but cannot determine original user"
            read -p "Enter username to run ExeClaw as: " INSTALL_USER
            INSTALL_HOME=$(eval echo ~"$INSTALL_USER")
        fi
    else
        INSTALL_USER="$USER"
        INSTALL_HOME="$HOME"
    fi
    
    # Create a temporary service file with correct paths
    TEMP_SERVICE=$(mktemp)
    sed -e "s|User=.*|User=$INSTALL_USER|" \
        -e "s|Group=.*|Group=$INSTALL_USER|" \
        -e "s|WorkingDirectory=.*|WorkingDirectory=$SCRIPT_DIR|" \
        -e "s|ExecStart=.*|ExecStart=$SCRIPT_DIR/execlaw|" \
        -e "s|Environment=HOME=.*|Environment=HOME=$INSTALL_HOME|" \
        "$SCRIPT_DIR/execlaw.service" > "$TEMP_SERVICE"
    
    # Install service file
    print_info "Installing systemd service..."
    $SUDO_CMD cp "$TEMP_SERVICE" /etc/systemd/system/execlaw.service
    rm "$TEMP_SERVICE"
    
    # Reload systemd
    $SUDO_CMD systemctl daemon-reload
    print_success "Systemd service installed"
    
    # Enable service
    print_info "Enabling ExeClaw service..."
    $SUDO_CMD systemctl enable execlaw.service >/dev/null 2>&1
    print_success "ExeClaw service enabled"
    
    # Ask if user wants to start now
    read -p "Start ExeClaw service now? [Y/n] " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Nn]$ ]]; then
        $SUDO_CMD systemctl start execlaw.service
        sleep 2
        if $SUDO_CMD systemctl is-active --quiet execlaw.service; then
            print_success "ExeClaw service started successfully"
        else
            print_error "Failed to start ExeClaw service"
            print_info "Check logs with: sudo journalctl -u execlaw.service -f"
        fi
    else
        print_info "You can start the service later with: sudo systemctl start execlaw.service"
    fi
    
    echo
}

###############################################################################
# Print Success Message
###############################################################################

print_success_message() {
    print_banner "Installation Complete!"
    
    echo -e "${GREEN}ExeClaw has been successfully installed!${NC}"
    echo
    echo "Next steps:"
    echo
    echo "  1. Start the service (if not already started):"
    echo -e "     ${BLUE}sudo systemctl start execlaw.service${NC}"
    echo
    echo "  2. Check service status:"
    echo -e "     ${BLUE}sudo systemctl status execlaw.service${NC}"
    echo
    echo "  3. View logs:"
    echo -e "     ${BLUE}sudo journalctl -u execlaw.service -f${NC}"
    echo
    echo "  4. Access ExeClaw web interface:"
    if hostname -f | grep -q 'exe\.xyz'; then
        echo -e "     ${BLUE}DETECTED EXE.DEV VM!${NC}"
        echo -e "     ${BLUE}https://$(hostname -f)${NC}"
    fi
    echo
    
    # Try to get the server's IP
    SERVER_IP=$(curl -fsSL https://ifconfig.me)
    if [ -n "$SERVER_IP" ]; then
	echo "  If NOT on an exe.dev VM, use the IP (or your FQDN that resolves to the VM's IP):"
        echo -e "     ${BLUE}http://${SERVER_IP}:8000${NC}"
        echo
	echo -e "${YELLOW}NOTE! Non-exe.dev platform deployments are unprotected (no auth!) and no SSL termination${NC}"
	echo -e "${YELLOW}Caddy Server can proxy and terminate SSL to this service and recommend to use basic auth:${NC}"
	echo -e "${YELLOW}https://caddyserver.com/docs/install${NC}"
	echo
	echo -e "${YELLOW}*** YOU CAN SAFELY IGNORE THIS IF ON AN EXE.DEV VM! (unless you decide to make the VM public) ***${NC}"
	echo
    fi
    
    print_info "ExeClaw is configured to run on port 8000"
    print_info "VM network uses subnet 10.200.0.0/16"
    echo
    
    echo -e "${YELLOW}Important Notes (mostly only a concern on NON-exe.dev platforms)):${NC}"
    echo "  • Make sure port 8000 is accessible (check firewall rules)"
    echo "  • VMs require internet access - ensure NAT/MASQUERADE is working"
    echo "  • Each VM session gets isolated Firecracker microVM"
    echo
    
    echo -e "${GREEN}Happy hacking with ExeClaw! 🔥${NC}"
    echo
}

###############################################################################
# Main Installation Flow
###############################################################################

main() {
    echo
    print_banner "ExeClaw Installation Script"
    echo
    print_info "This script will install and configure ExeClaw on your system."
    print_info "Installation directory: $SCRIPT_DIR"
    echo
    
    check_prerequisites
    build_go_binary
    download_firecracker
    download_vm_kernel
    build_vm_rootfs
    configure_network
    install_systemd_service
    print_success_message
}

# Run main installation
main
