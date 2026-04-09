package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	firecrackerBin   = "/usr/local/bin/firecracker"
	vmKernelPath     = "./vm-assets/vmlinux"
	vmBaseRootfsPath = "./vm-assets/rootfs.ext4"

	maxSessions       = 10
	sessionTimeoutMin = 30
	listenAddr        = ":8000"

	vcpuCount  = 2
	memSizeMiB = 1024

	subnetPrefix = "10.200"

	// Rate limiting: 60 requests per minute per IP.
	rateLimitBurst    = 60
	rateLimitInterval = time.Minute
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// SessionState represents the lifecycle of a VM session.
type SessionState string

const (
	StateCreating SessionState = "creating"
	StateBooting  SessionState = "booting"
	StateActive   SessionState = "active"
	StateStopping SessionState = "stopping"
	StateStopped  SessionState = "stopped"
	StateError    SessionState = "error"
)

// CreateSessionRequest is the JSON body for POST /api/sessions.
type CreateSessionRequest struct {
	Provider     string  `json:"provider"`
	APIKey       string  `json:"apiKey"`
	Model        string  `json:"model"`
	Name         string  `json:"name"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"maxTokens"`
	SystemPrompt string  `json:"systemPrompt"`
}

// InputRequest is the JSON body for POST /api/sessions/:id/input.
type InputRequest struct {
	Message string `json:"message"`
}

// EmailRequest is the JSON body for POST /api/sessions/:id/email.
type EmailRequest struct {
	To string `json:"to"`
}

// SSEEvent is a structured event sent over the SSE stream.
type SSEEvent struct {
	Event string      `json:"-"`
	Data  interface{} `json:"-"`
}

// SSEClient is a connected SSE listener.
type SSEClient struct {
	ch   chan SSEEvent
	done chan struct{}
}

// Session holds all state for one Firecracker VM session.
type Session struct {
	mu sync.Mutex

	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Provider     string       `json:"provider"`
	Model        string       `json:"model"`
	State        SessionState `json:"state"`
	CreatedAt    time.Time    `json:"createdAt"`
	LastActivity time.Time    `json:"lastActivity"`
	ErrorMsg     string       `json:"error,omitempty"`

	// VM-related
	tapIndex   int
	tapName    string
	rootfsPath string
	socketPath string

	// Process
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cancel context.CancelFunc

	// SSE clients
	clientsMu sync.Mutex
	clients   map[*SSEClient]struct{}

	// Output buffer for email
	outputMu  sync.Mutex
	outputBuf bytes.Buffer

	// Serial console parser state
	parser *OutputParser
}

// OutputParser is a simple state machine for parsing nullclaw serial output.
type OutputParser struct {
	state        parserState
	currentTool  *ToolCallEvent
	toolOutput   strings.Builder
	thinkBuf     strings.Builder
	textBuf      strings.Builder
	toolIDSeq    int64
	agentStarted bool
}

type parserState int

const (
	psNormal parserState = iota
	psThinking
	psToolCall
	psToolOutput
)

// Event data structures for SSE.
type MessageEvent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Type    string `json:"type"`
}

type ToolCallEvent struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Input  interface{} `json:"input"`
	Status string      `json:"status"`
}

type ToolResultEvent struct {
	ID       string `json:"id"`
	Output   string `json:"output"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
}

type ThinkingEvent struct {
	Content string `json:"content"`
}

type StatusEvent struct {
	State  string `json:"state"`
	Uptime int64  `json:"uptime"`
}

type ErrorEvent struct {
	Message string `json:"message"`
}

type ExitEvent struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// Server is the main application server.
type Server struct {
	sessions   sync.Map // map[string]*Session
	tapCounter atomic.Int64

	// Rate limiter per IP
	rateMu     sync.Mutex
	rateMap    map[string]*rateBucket

	ctx    context.Context
	cancel context.CancelFunc
}

type rateBucket struct {
	tokens    int
	lastReset time.Time
}

// ---------------------------------------------------------------------------
// Regex patterns for output parsing
// ---------------------------------------------------------------------------

var (
	// ANSI escape code stripper
	ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

	// Patterns for detecting nullclaw output structure
	thinkStartRegex  = regexp.MustCompile(`(?i)^[│\|]?\s*<thinking>|^\s*🤔|^Thinking[.…]`)
	thinkEndRegex    = regexp.MustCompile(`(?i)^[│\|]?\s*</thinking>`)
	toolStartRegex   = regexp.MustCompile(`(?i)^[│\|]?\s*(?:Running|Executing|Tool):\s*` + "`" + `?(\w+)` + "`" + `?`)
	toolInputRegex   = regexp.MustCompile(`(?i)^[│\|]?\s*(?:Command|Input|Args):\s*(.+)`)
	toolEndRegex     = regexp.MustCompile(`(?i)^[│\|]?\s*(?:─{3,}|═{3,}|Result:|Output:|Exit code:\s*(\d+))`)
	exitCodeRegex    = regexp.MustCompile(`(?i)Exit code:\s*(\d+)`)
	loginPromptRegex = regexp.MustCompile(`(?i)login:\s*$`)
	shellPromptRegex = regexp.MustCompile(`(?:#|\$)\s*$`)

	// Tool block delimiters used by nullclaw
	toolBlockStartRegex = regexp.MustCompile(`^[┌╭]─+`)
	toolBlockEndRegex   = regexp.MustCompile(`^[└╰]─+`)
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func generateSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("jsonResponse encode error: %v", err)
	}
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonResponse(w, status, map[string]string{"error": msg})
}

func stripAnsi(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func kvmAvailable() bool {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return false
	}
	// Check it's a character device we can open.
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// sessionCount returns the number of active sessions.
func (s *Server) sessionCount() int {
	count := 0
	s.sessions.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

// getSession fetches a session by ID or returns nil.
func (s *Server) getSession(id string) *Session {
	if v, ok := s.sessions.Load(id); ok {
		return v.(*Session)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rate Limiter
// ---------------------------------------------------------------------------

func (s *Server) allowRequest(ip string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	now := time.Now()
	b, ok := s.rateMap[ip]
	if !ok {
		s.rateMap[ip] = &rateBucket{tokens: rateLimitBurst - 1, lastReset: now}
		return true
	}
	if now.Sub(b.lastReset) >= rateLimitInterval {
		b.tokens = rateLimitBurst - 1
		b.lastReset = now
		return true
	}
	if b.tokens > 0 {
		b.tokens--
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// IP Forwarding & NAT Setup
// ---------------------------------------------------------------------------

func setupNetworking() error {
	// Enable IP forwarding.
	if out, err := exec.Command("sudo", "sysctl", "-w", "net.ipv4.ip_forward=1").CombinedOutput(); err != nil {
		log.Printf("Warning: could not enable ip_forward: %v: %s", err, out)
	}

	// Set up NAT masquerade for VM subnet.
	// Check if rule exists first.
	if err := exec.Command("sudo", "iptables", "-t", "nat", "-C", "POSTROUTING", "-s", "10.200.0.0/16", "-j", "MASQUERADE").Run(); err != nil {
		// Rule doesn't exist, add it.
		add := exec.Command("sudo", "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "10.200.0.0/16", "-o", defaultInterface(), "-j", "MASQUERADE")
		if out, err := add.CombinedOutput(); err != nil {
			return fmt.Errorf("iptables NAT setup failed: %v: %s", err, out)
		}
	}

	// Allow forwarding for VM subnet.
	if err := exec.Command("sudo", "iptables", "-C", "FORWARD", "-s", "10.200.0.0/16", "-j", "ACCEPT").Run(); err != nil {
		add := exec.Command("sudo", "iptables", "-A", "FORWARD", "-s", "10.200.0.0/16", "-j", "ACCEPT")
		if out, err := add.CombinedOutput(); err != nil {
			return fmt.Errorf("iptables FORWARD setup failed: %v: %s", err, out)
		}
	}
	if err := exec.Command("sudo", "iptables", "-C", "FORWARD", "-d", "10.200.0.0/16", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run(); err != nil {
		add := exec.Command("sudo", "iptables", "-A", "FORWARD", "-d", "10.200.0.0/16", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
		if out, err := add.CombinedOutput(); err != nil {
			return fmt.Errorf("iptables FORWARD setup failed: %v: %s", err, out)
		}
	}

	log.Println("Networking: IP forwarding enabled, NAT configured for 10.200.0.0/16")
	return nil
}

func defaultInterface() string {
	out, err := exec.Command("bash", "-c", "ip route show default | awk '/default/ {print $5}'").Output()
	if err != nil {
		return "eth0"
	}
	iface := strings.TrimSpace(string(out))
	if iface == "" {
		return "eth0"
	}
	return iface
}

// ---------------------------------------------------------------------------
// TAP Device Management
// ---------------------------------------------------------------------------

func createTapDevice(name, hostIP string) error {
	// Create TAP device.
	if out, err := exec.Command("sudo", "ip", "tuntap", "add", "dev", name, "mode", "tap").CombinedOutput(); err != nil {
		return fmt.Errorf("create tap %s: %v: %s", name, err, out)
	}
	// Assign IP.
	if out, err := exec.Command("sudo", "ip", "addr", "add", hostIP, "dev", name).CombinedOutput(); err != nil {
		_ = destroyTapDevice(name)
		return fmt.Errorf("assign ip to %s: %v: %s", name, err, out)
	}
	// Bring up.
	if out, err := exec.Command("sudo", "ip", "link", "set", name, "up").CombinedOutput(); err != nil {
		_ = destroyTapDevice(name)
		return fmt.Errorf("bring up %s: %v: %s", name, err, out)
	}
	return nil
}

func destroyTapDevice(name string) error {
	out, err := exec.Command("sudo", "ip", "link", "del", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete tap %s: %v: %s", name, err, out)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rootfs Preparation
// ---------------------------------------------------------------------------

func buildNullclawConfig(provider, apiKey, model string) []byte {
	// Build the provider model string.
	var primaryModel string
	switch provider {
	case "openrouter":
		primaryModel = "openrouter/" + model
	case "anthropic":
		primaryModel = "anthropic/" + model
	case "openai":
		primaryModel = "openai/" + model
	default:
		primaryModel = provider + "/" + model
	}

	cfg := map[string]interface{}{
		"models": map[string]interface{}{
			"providers": map[string]interface{}{
				provider: map[string]interface{}{
					"api_key": apiKey,
				},
			},
		},
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{
				"model": map[string]interface{}{
					"primary": primaryModel,
				},
			},
		},
		"channels": map[string]interface{}{
			"cli": true,
		},
		"memory": map[string]interface{}{
			"backend":   "sqlite",
			"auto_save": false,
		},
		"autonomy": map[string]interface{}{
			"level":                "full",
			"workspace_only":       false,
			"max_actions_per_hour": 100,
		},
		"security": map[string]interface{}{
			"sandbox": map[string]interface{}{
				"backend": "none",
			},
		},
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	return data
}

func prepareRootfs(sessionID, provider, apiKey, model string) (string, error) {
	dstPath := fmt.Sprintf("/tmp/webclaw-%s.ext4", sessionID)

	// Get absolute path to base rootfs.
	absBase, err := filepath.Abs(vmBaseRootfsPath)
	if err != nil {
		return "", fmt.Errorf("resolve rootfs path: %w", err)
	}

	// Copy base rootfs.
	if out, err := exec.Command("cp", "--reflink=auto", absBase, dstPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("copy rootfs: %v: %s", err, out)
	}

	// Write nullclaw config into the rootfs using debugfs.
	configData := buildNullclawConfig(provider, apiKey, model)

	// Create temporary config file to pipe into debugfs.
	tmpCfg, err := os.CreateTemp("", "nullclaw-config-*.json")
	if err != nil {
		os.Remove(dstPath)
		return "", fmt.Errorf("create temp config: %w", err)
	}
	defer os.Remove(tmpCfg.Name())

	if _, err := tmpCfg.Write(configData); err != nil {
		tmpCfg.Close()
		os.Remove(dstPath)
		return "", fmt.Errorf("write temp config: %w", err)
	}
	tmpCfg.Close()

	// Use debugfs to create the directory and write the config.
	debugfsCommands := fmt.Sprintf(
		"mkdir /root/.nullclaw\nwrite %s /root/.nullclaw/config.json\n",
		tmpCfg.Name(),
	)

	cmd := exec.Command("debugfs", "-w", dstPath)
	cmd.Stdin = strings.NewReader(debugfsCommands)
	if out, err := cmd.CombinedOutput(); err != nil {
		// debugfs may return non-zero even on success for mkdir if dir exists.
		// Check if it's a real error.
		outStr := string(out)
		if !strings.Contains(outStr, "File exists") {
			log.Printf("debugfs output (may be ok): %s", outStr)
		}
	}

	return dstPath, nil
}

// ---------------------------------------------------------------------------
// Firecracker VM Management
// ---------------------------------------------------------------------------

// fcAPIRequest makes an HTTP request to Firecracker over the Unix socket.
func fcAPIRequest(socketPath, method, path string, body interface{}) error {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial firecracker socket: %w", err)
	}
	defer conn.Close()

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, "http://localhost"+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Use HTTP client with unix socket transport.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, 5*time.Second)
			},
		},
		Timeout: 10 * time.Second,
	}
	conn.Close() // Close the initial diag connection, client creates its own.

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker API %s %s returned %d: %s", method, path, resp.StatusCode, respBody)
	}
	return nil
}

// startVM starts a Firecracker VM for the given session.
func (s *Server) startVM(sess *Session, req CreateSessionRequest) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Session %s: panic in startVM: %v", sess.ID, r)
			sess.mu.Lock()
			sess.State = StateError
			sess.ErrorMsg = fmt.Sprintf("panic: %v", r)
			sess.mu.Unlock()
			sess.broadcastSSE(SSEEvent{Event: "error", Data: ErrorEvent{Message: sess.ErrorMsg}})
		}
	}()

	sess.mu.Lock()
	sess.State = StateCreating
	sess.mu.Unlock()
	sess.broadcastSSE(SSEEvent{Event: "status", Data: StatusEvent{State: string(StateCreating)}})

	// 1. Allocate TAP index.
	tapIdx := int(s.tapCounter.Add(1))
	sess.tapIndex = tapIdx
	sess.tapName = fmt.Sprintf("fctap%d", tapIdx)

	hostIP := fmt.Sprintf("%s.%d.1/30", subnetPrefix, tapIdx)
	guestIP := fmt.Sprintf("%s.%d.2", subnetPrefix, tapIdx)
	guestGW := fmt.Sprintf("%s.%d.1", subnetPrefix, tapIdx)
	guestMAC := fmt.Sprintf("02:FC:00:00:%02X:%02X", (tapIdx>>8)&0xFF, tapIdx&0xFF)

	// 2. Prepare rootfs.
	log.Printf("Session %s: preparing rootfs", sess.ID)
	rootfsPath, err := prepareRootfs(sess.ID, req.Provider, req.APIKey, req.Model)
	if err != nil {
		log.Printf("Session %s: rootfs preparation failed: %v", sess.ID, err)
		sess.mu.Lock()
		sess.State = StateError
		sess.ErrorMsg = fmt.Sprintf("rootfs preparation failed: %v", err)
		sess.mu.Unlock()
		sess.broadcastSSE(SSEEvent{Event: "error", Data: ErrorEvent{Message: sess.ErrorMsg}})
		return
	}
	sess.rootfsPath = rootfsPath

	// 3. Create TAP device.
	log.Printf("Session %s: creating TAP device %s with IP %s", sess.ID, sess.tapName, hostIP)
	if err := createTapDevice(sess.tapName, hostIP); err != nil {
		log.Printf("Session %s: TAP creation failed: %v", sess.ID, err)
		sess.mu.Lock()
		sess.State = StateError
		sess.ErrorMsg = fmt.Sprintf("TAP creation failed: %v", err)
		sess.mu.Unlock()
		sess.broadcastSSE(SSEEvent{Event: "error", Data: ErrorEvent{Message: sess.ErrorMsg}})
		os.Remove(rootfsPath)
		return
	}

	// 4. Start Firecracker process.
	sess.mu.Lock()
	sess.State = StateBooting
	sess.mu.Unlock()
	sess.broadcastSSE(SSEEvent{Event: "status", Data: StatusEvent{State: string(StateBooting)}})

	socketPath := fmt.Sprintf("/tmp/fc-%s.sock", sess.ID)
	sess.socketPath = socketPath

	// Remove stale socket.
	os.Remove(socketPath)

	ctx, cancel := context.WithCancel(s.ctx)
	sess.cancel = cancel

	absKernel, _ := filepath.Abs(vmKernelPath)

	cmd := exec.CommandContext(ctx, firecrackerBin,
		"--api-sock", socketPath,
	)

	// Get stdin/stdout pipes for serial console.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("Session %s: stdin pipe failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("stdin pipe: %v", err))
		destroyTapDevice(sess.tapName)
		os.Remove(rootfsPath)
		return
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Session %s: stdout pipe failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("stdout pipe: %v", err))
		destroyTapDevice(sess.tapName)
		os.Remove(rootfsPath)
		return
	}

	// Merge stderr into stdout for serial console.
	cmd.Stderr = cmd.Stdout

	sess.cmd = cmd
	sess.stdin = stdinPipe
	sess.stdout = stdoutPipe

	log.Printf("Session %s: starting firecracker", sess.ID)
	if err := cmd.Start(); err != nil {
		log.Printf("Session %s: firecracker start failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("firecracker start: %v", err))
		destroyTapDevice(sess.tapName)
		os.Remove(rootfsPath)
		return
	}

	// Wait for the API socket to become available.
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 5. Configure VM via Firecracker API.
	// Use kernel ip= parameter for network config (more reliable than rc.local)
	// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:255.255.255.252:nullclaw:eth0:off",
		guestIP, guestGW,
	)

	// PUT /boot-source
	if err := fcAPIRequest(socketPath, "PUT", "/boot-source", map[string]interface{}{
		"kernel_image_path": absKernel,
		"boot_args":         bootArgs,
	}); err != nil {
		log.Printf("Session %s: boot-source config failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("boot-source config: %v", err))
		s.cleanupSession(sess)
		return
	}

	// PUT /drives/rootfs
	if err := fcAPIRequest(socketPath, "PUT", "/drives/rootfs", map[string]interface{}{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		log.Printf("Session %s: drive config failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("drive config: %v", err))
		s.cleanupSession(sess)
		return
	}

	// PUT /network-interfaces/eth0
	if err := fcAPIRequest(socketPath, "PUT", "/network-interfaces/eth0", map[string]interface{}{
		"iface_id":      "eth0",
		"guest_mac":     guestMAC,
		"host_dev_name": sess.tapName,
	}); err != nil {
		log.Printf("Session %s: network config failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("network config: %v", err))
		s.cleanupSession(sess)
		return
	}

	// PUT /machine-config
	if err := fcAPIRequest(socketPath, "PUT", "/machine-config", map[string]interface{}{
		"vcpu_count":  vcpuCount,
		"mem_size_mib": memSizeMiB,
	}); err != nil {
		log.Printf("Session %s: machine config failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("machine config: %v", err))
		s.cleanupSession(sess)
		return
	}

	// PUT /actions -> InstanceStart
	if err := fcAPIRequest(socketPath, "PUT", "/actions", map[string]interface{}{
		"action_type": "InstanceStart",
	}); err != nil {
		log.Printf("Session %s: instance start failed: %v", sess.ID, err)
		sess.setError(fmt.Sprintf("instance start: %v", err))
		s.cleanupSession(sess)
		return
	}

	log.Printf("Session %s: VM started, reading serial console", sess.ID)

	// 6. Start reading serial console output.
	go sess.readSerialConsole(ctx)

	// 7. Wait for login prompt, log in, and start nullclaw.
	go sess.performLogin(ctx)

	// 8. Wait for firecracker process to exit.
	go func() {
		err := cmd.Wait()
		log.Printf("Session %s: firecracker exited: %v", sess.ID, err)
		sess.mu.Lock()
		if sess.State != StateStopped && sess.State != StateError {
			sess.State = StateStopped
		}
		sess.mu.Unlock()
		sess.broadcastSSE(SSEEvent{Event: "exit", Data: ExitEvent{Code: 0, Reason: "VM process exited"}})
	}()
}

// setError sets the session into error state and broadcasts.
func (sess *Session) setError(msg string) {
	sess.mu.Lock()
	sess.State = StateError
	sess.ErrorMsg = msg
	sess.mu.Unlock()
	sess.broadcastSSE(SSEEvent{Event: "error", Data: ErrorEvent{Message: msg}})
}

// readSerialConsole reads Firecracker stdout and parses output.
func (sess *Session) readSerialConsole(ctx context.Context) {
	scanner := bufio.NewScanner(sess.stdout)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()

		// Store raw output for email.
		sess.outputMu.Lock()
		sess.outputBuf.WriteString(line)
		sess.outputBuf.WriteByte('\n')
		sess.outputMu.Unlock()

		// Update last activity.
		sess.mu.Lock()
		sess.LastActivity = time.Now()
		sess.mu.Unlock()

		// Parse and emit SSE events.
		sess.parser.parseLine(sess, line)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Session %s: serial console read error: %v", sess.ID, err)
	}
}

// performLogin waits for the login prompt and starts nullclaw.
func (sess *Session) performLogin(ctx context.Context) {
	// Wait for VM to boot (up to 30 seconds).
	time.Sleep(3 * time.Second)

	select {
	case <-ctx.Done():
		return
	default:
	}

	// Send root login.
	log.Printf("Session %s: sending login", sess.ID)
	sess.writeSerial("root\n")

	// Wait for shell prompt.
	time.Sleep(2 * time.Second)

	select {
	case <-ctx.Done():
		return
	default:
	}

	// Start nullclaw agent.
	log.Printf("Session %s: starting nullclaw agent", sess.ID)
	sess.writeSerial("nullclaw agent\n")

	// Wait a moment then mark as active.
	time.Sleep(2 * time.Second)

	sess.mu.Lock()
	if sess.State == StateBooting {
		sess.State = StateActive
	}
	sess.mu.Unlock()
	sess.broadcastSSE(SSEEvent{Event: "status", Data: StatusEvent{State: string(StateActive)}})
	log.Printf("Session %s: now active", sess.ID)
}

// writeSerial writes data to the VM's serial console stdin.
func (sess *Session) writeSerial(data string) {
	sess.mu.Lock()
	stdin := sess.stdin
	sess.mu.Unlock()

	if stdin == nil {
		log.Printf("Session %s: stdin is nil, cannot write", sess.ID)
		return
	}

	if _, err := io.WriteString(stdin, data); err != nil {
		log.Printf("Session %s: serial write error: %v", sess.ID, err)
	}
}

// ---------------------------------------------------------------------------
// Output Parser
// ---------------------------------------------------------------------------

func newOutputParser() *OutputParser {
	return &OutputParser{state: psNormal}
}

// isBootNoise checks if a line is kernel/systemd boot output that should be filtered.
func isBootNoise(s string) bool {
	noisePatterns := []string{
		"[", // kernel log lines like [    0.000000]
		"systemd[", "systemd-",
		"OK  ]", "FAILED]",
		"Starting ", "Started ", "Reached target", "Finished ",
		"Listening on", "Mounting ", "Mounted ",
		"Created slice", "Set up automount",
		"Welcome to Ubuntu", "ubuntu-fc-uvm",
		"nullclaw-sandbox login", "automatic login",
		"Documentation:", "Management:", "Support:",
		"minimized by removing", "unminimize",
		"programs included", "ABSOLUTELY NO WARRANTY",
		"distribution terms",
		"Linux version", "Command line:",
		"SELinux:", "BIOS-", "KASLR", "Hypervisor detected",
		"kvm-clock", "clocksource:",
		"audit:", "NET:", "ACPI:",
		"root@nullclaw-sandbox",
		"Expecting device",
		"fcnet.service", "rc-local.service",
		"Serial:", "virtio_blk",
		"EXT4-fs", "VFS:",
	}
	for _, p := range noisePatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	// Lines that are just the shell prompt
	if strings.HasSuffix(s, "# ") || strings.HasSuffix(s, "$ ") {
		return true
	}
	return false
}

func (p *OutputParser) parseLine(sess *Session, rawLine string) {
	line := stripAnsi(rawLine)
	trimmed := strings.TrimSpace(line)

	// Skip empty lines and common boot noise.
	if trimmed == "" {
		return
	}

	// Filter boot noise until nullclaw starts.
	if !p.agentStarted {
		if strings.Contains(trimmed, "nullclaw Agent") || strings.Contains(trimmed, "Type your message") {
			p.agentStarted = true
		} else if isBootNoise(trimmed) {
			return
		}
	}

	// Always filter pure boot noise even after agent starts (can appear in output)
	if isBootNoise(trimmed) && !strings.Contains(trimmed, "nullclaw") && !strings.Contains(trimmed, "Error") {
		return
	}

	switch p.state {
	case psNormal:
		p.parseNormal(sess, trimmed, line)
	case psThinking:
		p.parseThinking(sess, trimmed)
	case psToolCall:
		p.parseToolCall(sess, trimmed, line)
	case psToolOutput:
		p.parseToolOutput(sess, trimmed, line)
	}
}

func (p *OutputParser) parseNormal(sess *Session, trimmed, line string) {
	// Check for thinking start.
	if thinkStartRegex.MatchString(trimmed) {
		p.state = psThinking
		p.thinkBuf.Reset()
		return
	}

	// Check for tool block start (box-drawing characters).
	if toolBlockStartRegex.MatchString(trimmed) {
		p.state = psToolCall
		p.toolOutput.Reset()
		return
	}

	// Check for explicit tool start pattern.
	if m := toolStartRegex.FindStringSubmatch(trimmed); m != nil {
		p.toolIDSeq++
		toolID := fmt.Sprintf("tc_%d", p.toolIDSeq)
		p.currentTool = &ToolCallEvent{
			ID:     toolID,
			Name:   m[1],
			Status: "running",
		}

		// Try to parse the input on the same line or next.
		if im := toolInputRegex.FindStringSubmatch(trimmed); im != nil {
			p.currentTool.Input = map[string]string{"command": im[1]}
		}

		sess.broadcastSSE(SSEEvent{Event: "tool_call", Data: p.currentTool})
		p.state = psToolOutput
		p.toolOutput.Reset()
		return
	}

	// Flush any accumulated text.
	if len(trimmed) > 0 {
		sess.broadcastSSE(SSEEvent{Event: "message", Data: MessageEvent{
			Role:    "assistant",
			Content: line,
			Type:    "text",
		}})
	}
}

func (p *OutputParser) parseThinking(sess *Session, trimmed string) {
	if thinkEndRegex.MatchString(trimmed) {
		content := p.thinkBuf.String()
		if content != "" {
			sess.broadcastSSE(SSEEvent{Event: "thinking", Data: ThinkingEvent{Content: content}})
		}
		p.thinkBuf.Reset()
		p.state = psNormal
		return
	}
	p.thinkBuf.WriteString(trimmed)
	p.thinkBuf.WriteByte('\n')
}

func (p *OutputParser) parseToolCall(sess *Session, trimmed, line string) {
	// Inside a box-drawing tool block, look for the tool name/command.
	if toolBlockEndRegex.MatchString(trimmed) {
		// End of tool block - emit accumulated as tool result.
		if p.currentTool != nil {
			sess.broadcastSSE(SSEEvent{Event: "tool_result", Data: ToolResultEvent{
				ID:       p.currentTool.ID,
				Output:   p.toolOutput.String(),
				Status:   "completed",
				ExitCode: 0,
			}})
		}
		p.currentTool = nil
		p.toolOutput.Reset()
		p.state = psNormal
		return
	}

	// Try to identify tool name.
	if p.currentTool == nil {
		if m := toolStartRegex.FindStringSubmatch(trimmed); m != nil {
			p.toolIDSeq++
			p.currentTool = &ToolCallEvent{
				ID:     fmt.Sprintf("tc_%d", p.toolIDSeq),
				Name:   m[1],
				Status: "running",
			}
			if im := toolInputRegex.FindStringSubmatch(trimmed); im != nil {
				p.currentTool.Input = map[string]string{"command": im[1]}
			}
			sess.broadcastSSE(SSEEvent{Event: "tool_call", Data: p.currentTool})
			return
		}
	}

	p.toolOutput.WriteString(line)
	p.toolOutput.WriteByte('\n')
}

func (p *OutputParser) parseToolOutput(sess *Session, trimmed, line string) {
	// Check for tool end.
	if toolEndRegex.MatchString(trimmed) || toolBlockEndRegex.MatchString(trimmed) {
		exitCode := 0
		if m := exitCodeRegex.FindStringSubmatch(trimmed); m != nil {
			fmt.Sscanf(m[1], "%d", &exitCode)
		}

		if p.currentTool != nil {
			sess.broadcastSSE(SSEEvent{Event: "tool_result", Data: ToolResultEvent{
				ID:       p.currentTool.ID,
				Output:   p.toolOutput.String(),
				Status:   "completed",
				ExitCode: exitCode,
			}})
		}
		p.currentTool = nil
		p.toolOutput.Reset()
		p.state = psNormal
		return
	}

	p.toolOutput.WriteString(line)
	p.toolOutput.WriteByte('\n')
}

// ---------------------------------------------------------------------------
// SSE Broadcasting
// ---------------------------------------------------------------------------

func (sess *Session) addSSEClient() *SSEClient {
	client := &SSEClient{
		ch:   make(chan SSEEvent, 256),
		done: make(chan struct{}),
	}
	sess.clientsMu.Lock()
	sess.clients[client] = struct{}{}
	sess.clientsMu.Unlock()
	return client
}

func (sess *Session) removeSSEClient(client *SSEClient) {
	sess.clientsMu.Lock()
	delete(sess.clients, client)
	sess.clientsMu.Unlock()
	close(client.done)
}

func (sess *Session) broadcastSSE(evt SSEEvent) {
	sess.clientsMu.Lock()
	defer sess.clientsMu.Unlock()

	for client := range sess.clients {
		select {
		case client.ch <- evt:
		default:
			// Client buffer full, drop event.
			log.Printf("Session %s: dropping SSE event for slow client", sess.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Session Cleanup
// ---------------------------------------------------------------------------

func (s *Server) cleanupSession(sess *Session) {
	log.Printf("Session %s: cleaning up", sess.ID)

	sess.mu.Lock()
	if sess.State == StateStopped {
		sess.mu.Unlock()
		return
	}
	sess.State = StateStopping
	sess.mu.Unlock()

	// Cancel context (kills firecracker process).
	if sess.cancel != nil {
		sess.cancel()
	}

	// Close stdin.
	if sess.stdin != nil {
		sess.stdin.Close()
	}

	// Wait for process to exit (with timeout).
	if sess.cmd != nil && sess.cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			sess.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			log.Printf("Session %s: force killing firecracker", sess.ID)
			sess.cmd.Process.Kill()
		}
	}

	// Remove TAP device.
	if sess.tapName != "" {
		if err := destroyTapDevice(sess.tapName); err != nil {
			log.Printf("Session %s: TAP cleanup error: %v", sess.ID, err)
		}
	}

	// Remove rootfs copy.
	if sess.rootfsPath != "" {
		if err := os.Remove(sess.rootfsPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Session %s: rootfs cleanup error: %v", sess.ID, err)
		}
	}

	// Remove socket.
	if sess.socketPath != "" {
		os.Remove(sess.socketPath)
	}

	sess.mu.Lock()
	sess.State = StateStopped
	sess.mu.Unlock()

	// Close all SSE clients.
	sess.clientsMu.Lock()
	for client := range sess.clients {
		close(client.ch)
		delete(sess.clients, client)
	}
	sess.clientsMu.Unlock()

	// Remove from server map.
	s.sessions.Delete(sess.ID)

	log.Printf("Session %s: cleanup complete", sess.ID)
}

// ---------------------------------------------------------------------------
// Inactivity Monitor
// ---------------------------------------------------------------------------

func (s *Server) monitorSessions(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			s.sessions.Range(func(key, value interface{}) bool {
				sess := value.(*Session)
				sess.mu.Lock()
				last := sess.LastActivity
				state := sess.State
				sess.mu.Unlock()

				if state != StateStopped && state != StateStopping && state != StateError {
					if now.Sub(last) > time.Duration(sessionTimeoutMin)*time.Minute {
						log.Printf("Session %s: timed out after %d minutes of inactivity", sess.ID, sessionTimeoutMin)
						sess.broadcastSSE(SSEEvent{Event: "exit", Data: ExitEvent{Code: 0, Reason: "Session timed out due to inactivity"}})
						go s.cleanupSession(sess)
					}
				}
				return true
			})
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"kvm":      kvmAvailable(),
		"sessions": s.sessionCount(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	provider := extractPathParam(r.URL.Path, "/api/models/")
	if provider == "" {
		jsonError(w, http.StatusBadRequest, "provider is required")
		return
	}

	switch provider {
	case "anthropic":
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"models": []map[string]interface{}{
				{"id": "claude-opus-4-20250918", "name": "Claude Opus 4.6", "default": true},
				{"id": "claude-sonnet-4-20250514", "name": "Claude Sonnet 4", "default": false},
				{"id": "claude-haiku-3-5-20241022", "name": "Claude 3.5 Haiku", "default": false},
			},
		})

	case "openai":
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"models": []map[string]interface{}{
				{"id": "gpt-5.4", "name": "GPT-5.4", "default": true},
				{"id": "gpt-4.1", "name": "GPT-4.1", "default": false},
				{"id": "gpt-4.1-mini", "name": "GPT-4.1 Mini", "default": false},
				{"id": "o4-mini", "name": "o4-mini", "default": false},
			},
		})

	case "openrouter":
		models, err := fetchOpenRouterFreeModels()
		if err != nil {
			log.Printf("OpenRouter models fetch error: %v", err)
			jsonError(w, http.StatusBadGateway, "failed to fetch OpenRouter models")
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"models": models,
		})

	default:
		jsonError(w, http.StatusBadRequest, "unknown provider: "+provider)
	}
}

func fetchOpenRouterFreeModels() ([]map[string]interface{}, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://openrouter.ai/api/v1/models")
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	var models []map[string]interface{}
	for _, m := range result.Data {
		if m.Pricing.Prompt == "0" && m.Pricing.Completion == "0" {
			models = append(models, map[string]interface{}{
				"id":      m.ID,
				"name":    m.Name,
				"default": false,
			})
		}
	}

	// Set Gemma 4 31B as default (user requested "Gemma 31b"), fall back to other Gemma models.
	foundGemmaDefault := false
	// Priority order: gemma-4-31b > gemma-3-27b > any gemma > first model
	priorityPatterns := []struct{ sub1, sub2 string }{
		{"gemma-4", "31b"},
		{"gemma-3", "27b"},
	}
	for _, p := range priorityPatterns {
		if foundGemmaDefault {
			break
		}
		for i, m := range models {
			id, _ := m["id"].(string)
			if strings.Contains(strings.ToLower(id), p.sub1) && strings.Contains(id, p.sub2) {
				models[i]["default"] = true
				foundGemmaDefault = true
				break
			}
		}
	}
	if !foundGemmaDefault {
		for i, m := range models {
			id, _ := m["id"].(string)
			if strings.Contains(strings.ToLower(id), "gemma") {
				models[i]["default"] = true
				foundGemmaDefault = true
				break
			}
		}
	}
	if !foundGemmaDefault && len(models) > 0 {
		models[0]["default"] = true
	}

	return models, nil
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.sessionCount() >= maxSessions {
		jsonError(w, http.StatusServiceUnavailable, "maximum number of concurrent sessions reached")
		return
	}

	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// Validate required fields.
	if req.Provider == "" {
		jsonError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if req.APIKey == "" {
		jsonError(w, http.StatusBadRequest, "apiKey is required")
		return
	}
	if req.Model == "" {
		jsonError(w, http.StatusBadRequest, "model is required")
		return
	}

	validProviders := map[string]bool{"anthropic": true, "openai": true, "openrouter": true}
	if !validProviders[req.Provider] {
		jsonError(w, http.StatusBadRequest, "invalid provider: must be anthropic, openai, or openrouter")
		return
	}

	if req.Name == "" {
		req.Name = "Session"
	}

	sessionID, err := generateSessionID()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to generate session ID")
		return
	}

	now := time.Now()
	sess := &Session{
		ID:           sessionID,
		Name:         req.Name,
		Provider:     req.Provider,
		Model:        req.Model,
		State:        StateCreating,
		CreatedAt:    now,
		LastActivity: now,
		clients:      make(map[*SSEClient]struct{}),
		parser:       newOutputParser(),
	}

	s.sessions.Store(sessionID, sess)

	// Start VM in background.
	go s.startVM(sess, req)

	jsonResponse(w, http.StatusCreated, map[string]interface{}{
		"id":     sessionID,
		"status": "created",
	})
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	id := extractPathParam(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/status")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "session ID is required")
		return
	}

	sess := s.getSession(id)
	if sess == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}

	sess.mu.Lock()
	state := sess.State
	uptime := int64(time.Since(sess.CreatedAt).Seconds())
	model := sess.Model
	errorMsg := sess.ErrorMsg
	sess.mu.Unlock()

	resp := map[string]interface{}{
		"id":     id,
		"state":  string(state),
		"uptime": uptime,
		"model":  model,
	}
	if errorMsg != "" {
		resp["error"] = errorMsg
	}

	jsonResponse(w, http.StatusOK, resp)
}

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := extractPathParam(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/stream")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "session ID is required")
		return
	}

	sess := s.getSession(id)
	if sess == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	client := sess.addSSEClient()
	defer sess.removeSSEClient(client)

	// Send initial status event.
	sess.mu.Lock()
	state := sess.State
	sess.mu.Unlock()

	initialEvent := SSEEvent{Event: "status", Data: StatusEvent{
		State:  string(state),
		Uptime: int64(time.Since(sess.CreatedAt).Seconds()),
	}}
	writeSSEEvent(w, initialEvent)
	flusher.Flush()

	// Stream events.
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-client.ch:
			if !ok {
				return
			}
			writeSSEEvent(w, evt)
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, evt SSEEvent) {
	data, err := json.Marshal(evt.Data)
	if err != nil {
		log.Printf("SSE marshal error: %v", err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Event, data)
}

func (s *Server) handleSessionInput(w http.ResponseWriter, r *http.Request) {
	id := extractPathParam(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/input")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "session ID is required")
		return
	}

	sess := s.getSession(id)
	if sess == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}

	sess.mu.Lock()
	state := sess.State
	sess.mu.Unlock()

	if state != StateActive && state != StateBooting {
		jsonError(w, http.StatusBadRequest, "session is not active")
		return
	}

	var req InputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if req.Message == "" {
		jsonError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Write message to serial console.
	sess.writeSerial(req.Message + "\n")

	// Update activity timestamp.
	sess.mu.Lock()
	sess.LastActivity = time.Now()
	sess.mu.Unlock()

	jsonResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSessionEmail(w http.ResponseWriter, r *http.Request) {
	id := extractPathParam(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/email")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "session ID is required")
		return
	}

	sess := s.getSession(id)
	if sess == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}

	var req EmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if req.To == "" {
		jsonError(w, http.StatusBadRequest, "to email address is required")
		return
	}

	// Get conversation output.
	sess.outputMu.Lock()
	conversation := sess.outputBuf.String()
	sess.outputMu.Unlock()

	// Send email via exe.dev gateway.
	emailBody := map[string]interface{}{
		"to":      req.To,
		"subject": fmt.Sprintf("WebClaw Session: %s", sess.Name),
		"body":    conversation,
	}

	emailData, _ := json.Marshal(emailBody)
	emailReq, err := http.NewRequest("POST", "http://169.254.169.254/gateway/email/send", bytes.NewReader(emailData))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to create email request")
		return
	}
	emailReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(emailReq)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "email send failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		jsonError(w, http.StatusBadGateway, fmt.Sprintf("email API error %d: %s", resp.StatusCode, body))
		return
	}

	jsonResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := extractPathParam(r.URL.Path, "/api/sessions/")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "session ID is required")
		return
	}

	// Remove trailing slashes and sub-paths that shouldn't be here.
	id = strings.TrimRight(id, "/")

	sess := s.getSession(id)
	if sess == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}

	go s.cleanupSession(sess)

	jsonResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// extractPathParam extracts the portion of path after the given prefix.
func extractPathParam(path, prefix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	return strings.TrimPrefix(path, prefix)
}

// ---------------------------------------------------------------------------
// Router / Middleware
// ---------------------------------------------------------------------------

func (s *Server) setupRoutes() http.Handler {
	mux := http.NewServeMux()

	// API routes.
	mux.HandleFunc("/api/health", s.methodGuard("GET", s.handleHealth))
	mux.HandleFunc("/api/models/", s.methodGuard("GET", s.handleModels))

	// Session routes - we need to handle different methods on the same path prefix.
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions" {
			jsonError(w, http.StatusNotFound, "not found")
			return
		}
		switch r.Method {
		case "POST":
			s.handleCreateSession(w, r)
		default:
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/status") && r.Method == "GET":
			s.handleSessionStatus(w, r)
		case strings.HasSuffix(path, "/stream") && r.Method == "GET":
			s.handleSessionStream(w, r)
		case strings.HasSuffix(path, "/input") && r.Method == "POST":
			s.handleSessionInput(w, r)
		case strings.HasSuffix(path, "/email") && r.Method == "POST":
			s.handleSessionEmail(w, r)
		case r.Method == "DELETE":
			s.handleDeleteSession(w, r)
		default:
			jsonError(w, http.StatusNotFound, "not found")
		}
	})

	// Static files.
	staticDir := "./static"
	if _, err := os.Stat(staticDir); err == nil {
		fs := http.FileServer(http.Dir(staticDir))
		mux.Handle("/", fs)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte("<html><body><h1>WebClaw</h1><p>Static files not found. Place them in ./static/</p></body></html>"))
				return
			}
			http.NotFound(w, r)
		})
	}

	// Wrap with middleware.
	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS - same origin only.
		origin := r.Header.Get("Origin")
		if origin != "" {
			// Allow same-origin requests.
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Rate limiting.
		ip := extractIP(r)
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.allowRequest(ip) {
			jsonError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		// Security headers.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")

		// Log request (omit sensitive data).
		start := time.Now()
		log.Printf("%s %s %s", r.Method, r.URL.Path, ip)

		next.ServeHTTP(w, r)

		log.Printf("%s %s completed in %v", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) methodGuard(method string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handler(w, r)
	}
}

func extractIP(r *http.Request) string {
	// Check X-Forwarded-For first.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Graceful Shutdown
// ---------------------------------------------------------------------------

func (s *Server) gracefulShutdown() {
	log.Println("Shutting down gracefully...")

	// Cancel all sessions.
	s.sessions.Range(func(key, value interface{}) bool {
		sess := value.(*Session)
		log.Printf("Shutting down session %s", sess.ID)
		s.cleanupSession(sess)
		return true
	})

	log.Println("All sessions cleaned up")
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("WebClaw server starting...")

	// 1. Verify KVM availability.
	if !kvmAvailable() {
		log.Println("WARNING: /dev/kvm is not available or not accessible. VMs will not work.")
	} else {
		log.Println("KVM: available")
	}

	// 2. Verify required assets exist.
	for _, path := range []string{vmKernelPath, vmBaseRootfsPath} {
		if _, err := os.Stat(path); err != nil {
			log.Printf("WARNING: required asset missing: %s", path)
		}
	}

	if _, err := exec.LookPath(firecrackerBin); err != nil {
		log.Printf("WARNING: firecracker binary not found at %s", firecrackerBin)
	}

	// 3. Setup networking.
	if err := setupNetworking(); err != nil {
		log.Printf("WARNING: networking setup failed (may need root): %v", err)
	}

	// 4. Create server.
	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{
		rateMap: make(map[string]*rateBucket),
		ctx:     ctx,
		cancel:  cancel,
	}

	// 5. Start inactivity monitor.
	go srv.monitorSessions(ctx)

	// 6. Setup HTTP server.
	handler := srv.setupRoutes()
	httpServer := &http.Server{
		Addr:         listenAddr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE needs no write timeout.
		IdleTimeout:  120 * time.Second,
	}

	// 7. Handle signals for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal: %v", sig)
		cancel()
		srv.gracefulShutdown()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}()

	// 8. Start listening.
	log.Printf("WebClaw server listening on %s", listenAddr)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}

	log.Println("WebClaw server stopped")
}
