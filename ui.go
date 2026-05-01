package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── styles ────────────────────────────────────────────────────────────────────

var (
	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(0, 1)

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("62"))

	styleSelected = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))

	styleNormal = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	styleGood = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleBad  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	styleHelp = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	styleInput = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("212")).
			Padding(0, 1)
)

// ── model ─────────────────────────────────────────────────────────────────────

type uiMode int

const (
	modeList uiMode = iota
	modeAddName
	modeAddPort
	modeConfirmDelete
)

type statusInfo struct {
	daemonUp bool
	caddyUp  bool
	socksUp  bool
	tunnels  map[string]bool // name → up
}

type serviceEntry struct {
	name string
	port int
}

type model struct {
	mode     uiMode
	services []serviceEntry // sorted by name
	cursor   int

	// add flow
	inputName string
	inputPort string
	inputErr  string

	status statusInfo
}

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func fetchStatus(services []serviceEntry) statusInfo {
	s := statusInfo{
		daemonUp: launchdJobLoaded("com.proxima"),
		caddyUp:  tcpReachable("127.0.0.1:2019"),
		socksUp:  tcpReachable(socksAddr),
		tunnels:  make(map[string]bool),
	}
	for _, svc := range services {
		s.tunnels[svc.name] = tcpReachable(fmt.Sprintf("127.0.0.1:%d", localPort(svc.port)))
	}
	return s
}

type statusMsg statusInfo

func fetchStatusCmd(services []serviceEntry) tea.Cmd {
	return func() tea.Msg {
		return statusMsg(fetchStatus(services))
	}
}

func initialModel() model {
	cfg, _ := tryLoadConfig()
	svcs := configToEntries(cfg)
	return model{
		services: svcs,
		status:   fetchStatus(svcs),
	}
}

func configToEntries(cfg Config) []serviceEntry {
	var entries []serviceEntry
	for name, port := range cfg.Services {
		entries = append(entries, serviceEntry{name, port})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})
	return entries
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tick(), fetchStatusCmd(m.services))
}

// ── update ────────────────────────────────────────────────────────────────────

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tickMsg:
		return m, tea.Batch(tick(), fetchStatusCmd(m.services))

	case statusMsg:
		m.status = statusInfo(msg)
		return m, nil

	case tea.KeyMsg:
		switch m.mode {
		case modeList:
			return m.updateList(msg)
		case modeAddName:
			return m.updateAddName(msg)
		case modeAddPort:
			return m.updateAddPort(msg)
		case modeConfirmDelete:
			return m.updateConfirmDelete(msg)
		}
	}
	return m, nil
}

func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}

	case "down", "j":
		if m.cursor < len(m.services)-1 {
			m.cursor++
		}

	case "a":
		m.mode = modeAddName
		m.inputName = ""
		m.inputPort = ""
		m.inputErr = ""

	case "d":
		if len(m.services) > 0 {
			m.mode = modeConfirmDelete
		}

	case "o":
		if len(m.services) > 0 {
			svc := m.services[m.cursor]
			url := fmt.Sprintf("https://%s.dev.local", svc.name)
			exec.Command("open", url).Start() //nolint:errcheck
		}
	}
	return m, nil
}

func (m model) updateAddName(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
	case "enter":
		name := strings.TrimSpace(m.inputName)
		if name == "" {
			m.inputErr = "name cannot be empty"
			return m, nil
		}
		// Check duplicate.
		for _, s := range m.services {
			if s.name == name {
				m.inputErr = fmt.Sprintf("'%s' already exists", name)
				return m, nil
			}
		}
		m.inputErr = ""
		m.mode = modeAddPort
	case "backspace":
		if len(m.inputName) > 0 {
			m.inputName = m.inputName[:len(m.inputName)-1]
		}
	default:
		if len(msg.String()) == 1 {
			m.inputName += msg.String()
		}
	}
	return m, nil
}

func (m model) updateAddPort(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeAddName
	case "enter":
		port, err := strconv.Atoi(strings.TrimSpace(m.inputPort))
		if err != nil || port < 1 || port > 65535 {
			m.inputErr = "invalid port (1-65535)"
			return m, nil
		}
		// Save and restart.
		m.services = append(m.services, serviceEntry{m.inputName, port})
		sort.Slice(m.services, func(i, j int) bool {
			return m.services[i].name < m.services[j].name
		})
		m.cursor = 0
		m.mode = modeList
		m.inputErr = ""
		saveAndRestart(m.services)
		return m, fetchStatusCmd(m.services)
	case "backspace":
		if len(m.inputPort) > 0 {
			m.inputPort = m.inputPort[:len(m.inputPort)-1]
		}
	default:
		if len(msg.String()) == 1 && msg.String() >= "0" && msg.String() <= "9" {
			m.inputPort += msg.String()
		}
	}
	return m, nil
}

func (m model) updateConfirmDelete(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.services = append(m.services[:m.cursor], m.services[m.cursor+1:]...)
		if m.cursor >= len(m.services) && m.cursor > 0 {
			m.cursor--
		}
		m.mode = modeList
		saveAndRestart(m.services)
		return m, fetchStatusCmd(m.services)
	case "n", "N", "esc":
		m.mode = modeList
	}
	return m, nil
}

// ── view ──────────────────────────────────────────────────────────────────────

func (m model) View() string {
	var b strings.Builder

	// Header: system status.
	b.WriteString(styleTitle.Render("Proxima") + "\n")
	b.WriteString(statusLine("daemon", m.status.daemonUp) + "  ")
	b.WriteString(statusLine("caddy", m.status.caddyUp) + "  ")
	b.WriteString(statusLine("socks5", m.status.socksUp) + "\n\n")

	// Service table.
	if len(m.services) == 0 {
		b.WriteString(styleDim.Render("  no services — press [a] to add one") + "\n")
	} else {
		header := fmt.Sprintf("  %-18s %-12s %s", "SERVICE", "REMOTE PORT", "TUNNEL")
		b.WriteString(styleDim.Render(header) + "\n")
		b.WriteString(styleDim.Render("  " + strings.Repeat("─", 42)) + "\n")

		for i, svc := range m.services {
			tunnelUp := m.status.tunnels[svc.name]
			tunnel := styleGood.Render("✔ up")
			if !tunnelUp {
				tunnel = styleBad.Render("✗ down")
			}

			cursor := "  "
			if i == m.cursor {
				cursor = styleSelected.Render("▶ ")
			}

			line := fmt.Sprintf("%s%-18s %-12d %s", cursor, svc.name, svc.port, tunnel)
			if i == m.cursor {
				b.WriteString(styleSelected.Render(line) + "\n")
			} else {
				b.WriteString(styleNormal.Render(line) + "\n")
			}
		}
	}

	b.WriteString("\n")

	// Modal overlays.
	switch m.mode {
	case modeAddName:
		b.WriteString(m.renderInput("Add service — name:", m.inputName))
	case modeAddPort:
		b.WriteString(m.renderInput(fmt.Sprintf("Add '%s' — remote port:", m.inputName), m.inputPort))
	case modeConfirmDelete:
		if len(m.services) > 0 {
			svc := m.services[m.cursor]
			b.WriteString(styleBad.Render(fmt.Sprintf("  Delete '%s' (port %d)? [y/n]", svc.name, svc.port)) + "\n")
		}
	default:
		b.WriteString(styleHelp.Render("  [a]dd  [d]elete  [o]pen  [↑↓] select  [q]uit") + "\n")
	}

	return styleBorder.Render(b.String())
}

func (m model) renderInput(label, value string) string {
	var b strings.Builder
	b.WriteString(styleDim.Render("  "+label) + "\n")
	b.WriteString(styleInput.Render(value+"█") + "\n")
	if m.inputErr != "" {
		b.WriteString(styleBad.Render("  ✗ "+m.inputErr) + "\n")
	}
	b.WriteString(styleHelp.Render("  [enter] confirm  [esc] back") + "\n")
	return b.String()
}

func statusLine(label string, up bool) string {
	if up {
		return label + ": " + styleGood.Render("✔")
	}
	return label + ": " + styleBad.Render("✗")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// saveAndRestart writes config.json and runs proxima start in the background.
func saveAndRestart(services []serviceEntry) {
	cfg := Config{Services: make(map[string]int)}
	for _, s := range services {
		cfg.Services[s.name] = s.port
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return
	}

	// Get the path to the running proxima binary and restart in background.
	self, err := os.Executable()
	if err != nil {
		return
	}
	exec.Command(self, "start").Start() //nolint:errcheck
}

// ── entry point ───────────────────────────────────────────────────────────────

func runUI() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatalf("TUI error: %v", err)
	}
}
