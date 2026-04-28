package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config matches exactly: { "services": { "name": port, ... } }
type Config struct {
	Services map[string]int `json:"services"`
}

const socksAddr = "127.0.0.1:1080"

var (
	home       = os.Getenv("HOME")
	base       = filepath.Join(home, ".proxima")
	configPath = filepath.Join(base, "config.json")
	caddyFile  = filepath.Join(base, "Caddyfile")
	plistPath  = filepath.Join(home, "Library", "LaunchAgents", "com.proxima.caddy.plist")
)

// localPort returns a deterministic local port for a given remote port.
// local_port = 10000 + remote_port
func localPort(remotePort int) int {
	return 10000 + remotePort
}

func main() {
	cmd := "start"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "start":
		runStart()
	case "stop":
		runStop()
	case "status":
		runStatus()
	default:
		fmt.Fprintf(os.Stderr, "Usage: proxima [start|stop|status]\n")
		os.Exit(1)
	}
}

// ── start ────────────────────────────────────────────────────────────────────

func runStart() {
	fmt.Println("== Proxima ==")

	cfg := loadConfig()

	killOldGost()
	startGost(cfg)

	// Give gost processes a moment to bind their ports.
	time.Sleep(1 * time.Second)

	syncHosts(cfg)
	generateCaddyfile(cfg)
	reloadCaddy()

	fmt.Println("✔ Done")
}

// ── stop ─────────────────────────────────────────────────────────────────────

func runStop() {
	fmt.Println("== Proxima: stopping ==")

	// 1. Kill gost tunnels.
	fmt.Println("→ Killing gost processes")
	exec.Command("pkill", "-9", "-f", "gost -L=").Run() //nolint:errcheck

	// 2. Unload Caddy from launchd (stops the process and prevents auto-restart).
	fmt.Println("→ Stopping Caddy")
	out, err := exec.Command("launchctl", "unload", plistPath).CombinedOutput()
	if err != nil {
		// Not loaded is fine — just report unexpected errors.
		if !strings.Contains(string(out), "Could not find specified service") &&
			!strings.Contains(string(out), "No such file") {
			fmt.Printf("  ↳ launchctl unload: %s\n", strings.TrimSpace(string(out)))
		}
	} else {
		fmt.Println("  ↳ Caddy stopped")
	}

	// 3. Remove the proxima block from /etc/hosts.
	fmt.Println("→ Cleaning /etc/hosts")
	removeHostsBlock()

	fmt.Println("✔ Done")
}

// removeHostsBlock strips the proxima block from /etc/hosts.
func removeHostsBlock() {
	const blockStart = ">>> proxima start"
	const blockEnd = "<<< proxima end"

	raw, err := os.ReadFile("/etc/hosts")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ↳ cannot read /etc/hosts: %v\n", err)
		return
	}

	var kept []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, blockStart) {
			inBlock = true
			continue
		}
		if strings.Contains(line, blockEnd) {
			inBlock = false
			continue
		}
		if !inBlock {
			kept = append(kept, line)
		}
	}

	// Trim trailing blank lines.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	result := strings.Join(kept, "\n") + "\n"

	tmp := "/tmp/hosts.proxima"
	if err := os.WriteFile(tmp, []byte(result), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "  ↳ cannot write temp file: %v\n", err)
		return
	}
	out, err := exec.Command("sudo", "cp", tmp, "/etc/hosts").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ↳ sudo cp failed: %v\n%s\n", err, out)
		return
	}
	fmt.Println("  ↳ /etc/hosts cleaned")
}

// ── status ───────────────────────────────────────────────────────────────────

func runStatus() {
	fmt.Println("== Proxima: status ==")

	// Load config — soft fail so status works even with a broken config.
	cfg, err := tryLoadConfig()
	if err != nil {
		fmt.Printf("  config: ✗ %v\n\n", err)
	}

	// Caddy.
	caddyRunning := caddyIsRunning()
	if caddyRunning {
		fmt.Println("caddy:  ✔ running")
	} else {
		fmt.Println("caddy:  ✗ not running")
	}

	// SOCKS5 proxy.
	socksUp := portInUse2(socksAddr)
	if socksUp {
		fmt.Printf("socks5: ✔ reachable (%s)\n", socksAddr)
	} else {
		fmt.Printf("socks5: ✗ not reachable (%s)\n", socksAddr)
	}

	if err != nil || len(cfg.Services) == 0 {
		return
	}

	// Per-service tunnel status.
	fmt.Println()
	fmt.Printf("%-16s %-12s %-12s %s\n", "SERVICE", "REMOTE PORT", "LOCAL PORT", "TUNNEL")
	fmt.Println(strings.Repeat("─", 52))

	for name, remotePort := range cfg.Services {
		lp := localPort(remotePort)
		tunnelUp := portInUse2(fmt.Sprintf("127.0.0.1:%d", lp))
		status := "✔ up"
		if !tunnelUp {
			status = "✗ down"
		}
		fmt.Printf("%-16s %-12d %-12d %s\n", name, remotePort, lp, status)
	}
}

// caddyIsRunning checks whether Caddy's admin API is responding.
func caddyIsRunning() bool {
	return portInUse2("127.0.0.1:2019")
}

// portInUse2 checks whether a TCP address is reachable.
func portInUse2(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// tryLoadConfig loads config without calling fatalf — returns an error instead.
func tryLoadConfig() (Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("cannot read %s: %w", configPath, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("invalid JSON in %s: %w", configPath, err)
	}
	return cfg, nil
}

// ── shared helpers ────────────────────────────────────────────────────────────

// loadConfig reads ~/.proxima/config.json.
// It fails loudly if the file is missing or malformed.
func loadConfig() Config {
	if err := os.MkdirAll(base, 0755); err != nil {
		fatalf("cannot create config dir %s: %v", base, err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		fatalf("cannot read config %s: %v\n\nCreate it with:\n%s", configPath, err, exampleConfig())
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		fatalf("invalid config JSON in %s: %v", configPath, err)
	}

	if len(cfg.Services) == 0 {
		fatalf("config has no services defined in %s", configPath)
	}

	return cfg
}

func exampleConfig() string {
	return `{
  "services": {
    "myapp": 7777,
    "api": 3000
  }
}`
}

// killOldGost terminates any previously spawned gost processes.
// pkill failure is non-fatal (nothing to kill is fine).
func killOldGost() {
	fmt.Println("→ Killing old gost processes")
	// Match any gost process we may have spawned, regardless of mode.
	// -9 ensures they die even if they ignore SIGTERM.
	exec.Command("pkill", "-9", "-f", "gost -L=").Run() //nolint:errcheck
	// Give the OS a moment to release the ports before we check portInUse.
	time.Sleep(300 * time.Millisecond)
}

// portInUse returns true if something is already listening on the given TCP port.
func portInUse(port int) bool {
	return portInUse2(fmt.Sprintf("127.0.0.1:%d", port))
}

// startGost spawns one gost TCP-tunnel process per service.
//
// gost v3 TCP port forwarding syntax:
//
//	gost -L=tcp://127.0.0.1:<lp>/localhost:<remotePort> -F=socks5://127.0.0.1:1080
//
// The destination address (localhost:<remotePort>) is forwarded through the
// SOCKS5 chain, so "localhost" resolves on the REMOTE host — not locally.
// This is the correct way to reach a service on the remote machine's loopback.
func startGost(cfg Config) {
	fmt.Println("→ Starting gost processes")

	for name, remotePort := range cfg.Services {
		lp := localPort(remotePort)

		if portInUse(lp) {
			fmt.Printf("  ↳ %s: port %d already in use, skipping\n", name, lp)
			continue
		}

		// tcp://bind/destination — destination is resolved via the SOCKS5 proxy (remote side)
		listenAddr := fmt.Sprintf("tcp://127.0.0.1:%d/localhost:%d", lp, remotePort)
		forwardAddr := fmt.Sprintf("socks5://%s", socksAddr)

		cmd := exec.Command("gost", "-L="+listenAddr, "-F="+forwardAddr)

		// Declare svcName here so it's available for both the log filename and the goroutine.
		svcName := name
		logFile := filepath.Join(base, fmt.Sprintf("gost-%s.log", svcName))
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			cmd.Stdout = nil
			cmd.Stderr = nil
		} else {
			cmd.Stdout = f
			cmd.Stderr = f
		}

		fmt.Printf("  ↳ %s: local %d → SOCKS5 → remote localhost:%d\n", name, lp, remotePort)

		go func(c *exec.Cmd) {
			if err := c.Run(); err != nil {
				fmt.Printf("  gost exited (%s): %v\n", svcName, err)
			}
		}(cmd)
	}
}

// syncHosts rewrites /etc/hosts, replacing the proxima block entirely.
// Uses a temp file + sudo cp to avoid requiring the binary to run as root.
func syncHosts(cfg Config) {
	fmt.Println("→ Syncing /etc/hosts")

	const blockStart = ">>> proxima start"
	const blockEnd = "<<< proxima end"

	raw, err := os.ReadFile("/etc/hosts")
	if err != nil {
		fatalf("cannot read /etc/hosts: %v", err)
	}

	// Strip the old proxima block (if any).
	var kept []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, blockStart) {
			inBlock = true
			continue
		}
		if strings.Contains(line, blockEnd) {
			inBlock = false
			continue
		}
		if !inBlock {
			kept = append(kept, line)
		}
	}

	// Remove trailing blank lines so we get a clean join.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	// Build the new block.
	var block []string
	block = append(block, "")
	block = append(block, blockStart)
	for name := range cfg.Services {
		block = append(block, fmt.Sprintf("127.0.0.1 %s.dev.local", name))
	}
	block = append(block, blockEnd)
	block = append(block, "")

	result := strings.Join(append(kept, block...), "\n")

	tmp := "/tmp/hosts.proxima"
	if err := os.WriteFile(tmp, []byte(result), 0644); err != nil {
		fatalf("cannot write temp hosts file: %v", err)
	}

	out, err := exec.Command("sudo", "cp", tmp, "/etc/hosts").CombinedOutput()
	if err != nil {
		fatalf("sudo cp to /etc/hosts failed: %v\n%s", err, out)
	}
}

// generateCaddyfile writes ~/.proxima/Caddyfile and auto-formats it
// so Caddy doesn't emit the "not formatted" warning on load.
func generateCaddyfile(cfg Config) {
	fmt.Println("→ Generating Caddyfile")

	var sb strings.Builder

	for name, remotePort := range cfg.Services {
		lp := localPort(remotePort)
		// The gost TCP tunnel already routes to the correct remote port,
		// so Caddy just needs to set Host: localhost (no port suffix).
		// Many dev servers (Vite, Next.js, etc.) reject Host headers with ports.
		sb.WriteString(fmt.Sprintf(
			"https://%s.dev.local {\n"+
				"\treverse_proxy http://127.0.0.1:%d {\n"+
				"\t\theader_up Host localhost\n"+
				"\t}\n"+
				"}\n\n",
			name, lp,
		))
	}

	if err := os.WriteFile(caddyFile, []byte(sb.String()), 0644); err != nil {
		fatalf("cannot write Caddyfile: %v", err)
	}

	// Format in-place so Caddy doesn't warn about formatting on every reload.
	exec.Command("caddy", "fmt", "--overwrite", caddyFile).Run() //nolint:errcheck
}

// reloadCaddy tries `caddy reload` first; if Caddy isn't running it installs
// and loads the launchd plist so Caddy starts (and stays running).
func reloadCaddy() {
	fmt.Println("→ Reloading Caddy")

	reloadOut, err := exec.Command("caddy", "reload", "--config", caddyFile).CombinedOutput()
	if err == nil {
		fmt.Println("  ↳ caddy reloaded successfully")
		return
	}

	// Caddy is not running — set up launchd plist and start it.
	// Only show the reload error if it's something unexpected (not just "connection refused").
	if !strings.Contains(string(reloadOut), "connection refused") {
		fmt.Printf("  ↳ caddy reload: %s\n", strings.TrimSpace(string(reloadOut)))
	}
	fmt.Println("  ↳ caddy not running, installing launchd plist")

	caddyBin, err := exec.LookPath("caddy")
	if err != nil {
		fatalf("caddy binary not found in PATH: %v", err)
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.proxima.caddy</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
		<string>--config</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s/caddy.log</string>
	<key>StandardErrorPath</key>
	<string>%s/caddy.err</string>
</dict>
</plist>
`, caddyBin, caddyFile, base, base)

	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		fatalf("cannot create LaunchAgents dir: %v", err)
	}

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fatalf("cannot write plist: %v", err)
	}

	// Unload first (ignore error — it may not be loaded yet).
	exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck

	out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput()
	if err != nil {
		fatalf("launchctl load failed: %v\n%s", err, out)
	}

	fmt.Println("  ↳ caddy started via launchd")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "✗ "+format+"\n", args...)
	os.Exit(1)
}
