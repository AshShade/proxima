package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	logsDir    = filepath.Join(base, "logs")
	configPath = filepath.Join(base, "config.json")
	caddyFile  = filepath.Join(base, "Caddyfile")
	plistPath  = filepath.Join(home, "Library", "LaunchAgents", "com.proxima.plist")
)

// localPort returns the local tunnel port for a given remote port.
// Uses the same port to avoid issues with services that check port consistency.
func localPort(remotePort int) int {
	return remotePort
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
	case "daemon":
		runDaemon()
	case "ui":
		runUI()
	default:
		fmt.Fprintf(os.Stderr, "Usage: proxima [start|stop|status|ui]\n")
		os.Exit(1)
	}
}

// ── start ─────────────────────────────────────────────────────────────────────
// Prepares config, writes the launchd plist, and loads it.
// Returns immediately — launchd starts proxima daemon in the background.

func runStart() {
	fmt.Println("== Proxima: starting ==")

	cfg := loadConfig()

	fmt.Println("→ Syncing /etc/hosts")
	syncHosts(cfg)

	fmt.Println("→ Generating Caddyfile")
	generateCaddyfile(cfg)

	fmt.Println("→ Registering with launchd")
	proxBin, err := os.Executable()
	if err != nil {
		fatalf("cannot determine executable path: %v", err)
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.proxima</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s/daemon.log</string>
	<key>StandardErrorPath</key>
	<string>%s/daemon.log</string>
</dict>
</plist>
`, proxBin, logsDir, logsDir)

	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		fatalf("cannot create LaunchAgents dir: %v", err)
	}
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		fatalf("cannot create logs dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fatalf("cannot write plist: %v", err)
	}

	// Unload first so a re-run of start picks up any config changes.
	exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck
	time.Sleep(300 * time.Millisecond)

	out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput()
	if err != nil {
		fatalf("launchctl load failed: %v\n%s", err, out)
	}

	fmt.Println("✔ Done — proxima daemon started via launchd")
}

// ── stop ──────────────────────────────────────────────────────────────────────
// Unloads the launchd job (kills daemon + all children), cleans /etc/hosts.

func runStop() {
	fmt.Println("== Proxima: stopping ==")

	fmt.Println("→ Unloading launchd job")
	out, err := exec.Command("launchctl", "unload", plistPath).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if !strings.Contains(msg, "Could not find") && !strings.Contains(msg, "No such file") {
			fmt.Printf("  ↳ launchctl: %s\n", msg)
		}
	} else {
		fmt.Println("  ↳ daemon stopped")
	}

	// Belt-and-suspenders: kill any stray gost processes.
	exec.Command("pkill", "-9", "-f", "gost -L=").Run() //nolint:errcheck

	fmt.Println("→ Cleaning /etc/hosts")
	removeHostsBlock()

	fmt.Println("✔ Done")
}

// ── status ────────────────────────────────────────────────────────────────────

func runStatus() {
	fmt.Println("== Proxima: status ==")

	// Check if the launchd job is loaded.
	daemonLoaded := launchdJobLoaded("com.proxima")
	if daemonLoaded {
		fmt.Println("daemon: ✔ running (launchd)")
	} else {
		fmt.Println("daemon: ✗ not running")
	}

	// Caddy admin API.
	if tcpReachable("127.0.0.1:2019") {
		fmt.Println("caddy:  ✔ running")
	} else {
		fmt.Println("caddy:  ✗ not running")
	}

	// SOCKS5 proxy.
	if tcpReachable(socksAddr) {
		fmt.Printf("socks5: ✔ reachable (%s)\n", socksAddr)
	} else {
		fmt.Printf("socks5: ✗ not reachable (%s)\n", socksAddr)
	}

	// Per-service tunnel status.
	cfg, err := tryLoadConfig()
	if err != nil {
		fmt.Printf("\nconfig: ✗ %v\n", err)
		return
	}
	if len(cfg.Services) == 0 {
		return
	}

	fmt.Println()
	fmt.Printf("%-16s %-12s %-12s %s\n", "SERVICE", "REMOTE PORT", "LOCAL PORT", "TUNNEL")
	fmt.Println(strings.Repeat("─", 52))
	for name, remotePort := range cfg.Services {
		lp := localPort(remotePort)
		status := "✔ up"
		if !tcpReachable(fmt.Sprintf("127.0.0.1:%d", lp)) {
			status = "✗ down"
		}
		fmt.Printf("%-16s %-12d %-12d %s\n", name, remotePort, lp, status)
	}
}

// launchdJobLoaded returns true if the given launchd job label is loaded.
func launchdJobLoaded(label string) bool {
	out, err := exec.Command("launchctl", "list", label).CombinedOutput()
	if err != nil {
		return false
	}
	return !strings.Contains(string(out), "Could not find")
}

// ── daemon ────────────────────────────────────────────────────────────────────
// Long-running process managed by launchd.
// Spawns caddy and gost as children, truncates log files every hour, exits on SIGTERM.

func runDaemon() {
	cfg := loadConfig()

	// Fresh logs directory on every daemon start.
	os.RemoveAll(logsDir)
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		fatalf("cannot create logs dir: %v", err)
	}

	// Track all child processes for clean shutdown.
	var (
		mu       sync.Mutex
		children []*exec.Cmd
	)

	addChild := func(c *exec.Cmd) {
		mu.Lock()
		children = append(children, c)
		mu.Unlock()
	}

	killChildren := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range children {
			if c.Process != nil {
				c.Process.Kill() //nolint:errcheck
			}
		}
	}

	// Handle SIGTERM / SIGINT for clean shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		killChildren()
		os.Exit(0)
	}()

	// Start caddy and gost tunnels.
	addChild(startCaddy())
	for name, remotePort := range cfg.Services {
		if cmd := startGostTunnel(name, remotePort); cmd != nil {
			addChild(cmd)
		}
	}

	// Truncate all log files every hour on the hour.
	// Children keep their file handles open — truncating works fine on macOS/Linux;
	// the process continues writing from byte 0 after the truncation.
	go func() {
		for {
			now := time.Now()
			next := now.Truncate(time.Hour).Add(time.Hour)
			time.Sleep(time.Until(next))
			truncateLogs(cfg)
		}
	}()

	// Block forever (signal handler above handles exit).
	select {}
}

// truncateLogs wipes all log files to zero bytes every hour.
// Child processes keep their file handles open and continue writing from byte 0.
func truncateLogs(cfg Config) {
	names := []string{"caddy"}
	for name := range cfg.Services {
		names = append(names, "gost-"+name)
	}
	for _, name := range names {
		path := filepath.Join(logsDir, name+".log")
		if f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
			f.Close()
		}
	}
}

// openLog opens (or creates) the log file for the given name.
func openLog(name string) *os.File {
	f, err := os.OpenFile(
		filepath.Join(logsDir, name+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return nil
	}
	return f
}

// startCaddy spawns caddy run as a child process.
func startCaddy() *exec.Cmd {
	cmd := exec.Command("caddy", "run", "--config", caddyFile)
	if f := openLog("caddy"); f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "caddy start failed: %v\n", err)
		return cmd
	}
	go cmd.Wait() //nolint:errcheck
	return cmd
}

// startGostTunnel spawns a single gost TCP tunnel as a child process.
func startGostTunnel(name string, remotePort int) *exec.Cmd {
	lp := localPort(remotePort)
	listenAddr := fmt.Sprintf("tcp://127.0.0.1:%d/localhost:%d", lp, remotePort)
	forwardAddr := fmt.Sprintf("socks5://%s", socksAddr)

	cmd := exec.Command("gost", "-L="+listenAddr, "-F="+forwardAddr)
	if f := openLog("gost-" + name); f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gost start failed (%s): %v\n", name, err)
		return nil
	}
	go cmd.Wait() //nolint:errcheck
	return cmd
}

// ── /etc/hosts ────────────────────────────────────────────────────────────────

func syncHosts(cfg Config) {
	const blockStart = ">>> proxima start"
	const blockEnd = "<<< proxima end"

	raw, err := os.ReadFile("/etc/hosts")
	if err != nil {
		fatalf("cannot read /etc/hosts: %v", err)
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

	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

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

// ── Caddyfile ─────────────────────────────────────────────────────────────────

func generateCaddyfile(cfg Config) {
	var sb strings.Builder

	// Global block: log caddy's own output to the logs dir, hourly rotation.
	sb.WriteString(fmt.Sprintf(
		"{\n"+
			"\tlog {\n"+
			"\t\toutput file %s {\n"+
			"\t\t\troll_interval 1h\n"+
			"\t\t\troll_keep     2\n"+
			"\t\t\troll_keep_for 2h\n"+
			"\t\t}\n"+
			"\t}\n"+
			"}\n\n",
		filepath.Join(logsDir, "caddy.log"),
	))

	for name, remotePort := range cfg.Services {
		lp := localPort(remotePort)
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

	exec.Command("caddy", "fmt", "--overwrite", caddyFile).Run() //nolint:errcheck
}

// ── config ────────────────────────────────────────────────────────────────────

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

func exampleConfig() string {
	return `{
  "services": {
    "myapp": 7777,
    "api": 3000
  }
}`
}

// ── utilities ─────────────────────────────────────────────────────────────────

func tcpReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "✗ "+format+"\n", args...)
	os.Exit(1)
}
