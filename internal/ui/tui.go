package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/amglogicalis/permafrost/internal/diff"
	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TabIndex identifies the active screen in the Permafrost TUI.
type TabIndex int

const (
	TabSnapshots TabIndex = iota
	TabDiff
	TabFeatures
	TabAudit
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00E5FF")).
			Background(lipgloss.Color("#0B2545")).
			Padding(0, 2)

	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0B2545")).
			Background(lipgloss.Color("#00E5FF")).
			Padding(0, 2)

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#8DA9C4")).
				Background(lipgloss.Color("#13315C")).
				Padding(0, 2)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#1E6091")).
			Padding(1, 2)

	selectedRowStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#00F5D4"))

	greenBadge = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2EC4B6"))
	yellowText = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF9F1C"))
	redBadge   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E71D36"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C757D"))
	statusStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00F5D4")).
			Padding(0, 1)
)

// SwitchHandler allows Phase 3 Safe Switch & Hooks to wrap version switches seamlessly.
type SwitchHandler func(projectPath string, target *vault.Manifest, selectivePaths []string) (*engine.SwitchResult, string, error)

// FreezeHandler allows Phase 3 Hooks to wrap snapshot creation seamlessly.
type FreezeHandler func(projectPath, tag, message string) (*vault.Manifest, string, error)

// AppModel holds the interactive state of the Permafrost Bubble Tea TUI.
type AppModel struct {
	Vault         *vault.Vault
	Engine        *engine.Engine
	WorkingDir    string
	ActiveTab     TabIndex
	Projects      []*vault.ProjectRecord
	ProjectCursor int
	Snapshots     []*vault.Manifest
	SnapCursor    int
	DiffReport    *diff.Report
	DiffCursor    int
	SelectedFiles map[string]bool
	FeatCursor    int
	StatusMsg     string
	InputMode     bool
	InputTag      string
	InputMessage  string
	InputStep     int // 0: none, 1: tag, 2: message
	Width         int
	Height        int
	OnSwitch      SwitchHandler
	OnFreeze      FreezeHandler
}

// NewAppModel creates and initializes the TUI state for the current workspace.
func NewAppModel(v *vault.Vault, eng *engine.Engine, workDir string) *AppModel {
	m := &AppModel{
		Vault:         v,
		Engine:        eng,
		WorkingDir:    workDir,
		ActiveTab:     TabSnapshots,
		SelectedFiles: make(map[string]bool),
		Width:         100,
		Height:        30,
	}
	m.RefreshData()
	return m
}

// RefreshData reloads projects, snapshots, and diff preview from the Vault.
func (m *AppModel) RefreshData() {
	m.Projects = m.Vault.ListProjects()

	// Ensure current working directory is selected if registered
	if abs, err := vault.NormalizeProjectPath(m.WorkingDir); err == nil {
		id := vault.ComputeProjectID(abs)
		for i, p := range m.Projects {
			if p.ID == id {
				m.ProjectCursor = i
				break
			}
		}
	}

	if m.ProjectCursor >= len(m.Projects) {
		m.ProjectCursor = 0
	}

	if len(m.Projects) > 0 {
		activeProj := m.Projects[m.ProjectCursor]
		snaps, _ := m.Vault.ListManifests(activeProj.ID, false)
		m.Snapshots = snaps
		if m.SnapCursor >= len(m.Snapshots) {
			m.SnapCursor = 0
		}
		m.refreshDiff()
	} else {
		m.Snapshots = nil
		m.DiffReport = nil
	}
}

func (m *AppModel) refreshDiff() {
	if len(m.Projects) == 0 || len(m.Snapshots) == 0 {
		m.DiffReport = nil
		return
	}
	proj := m.Projects[m.ProjectCursor]
	snap := m.Snapshots[m.SnapCursor]
	rep, err := diff.CompareWorkingTreeToSnapshot(m.Engine, proj.Path, snap, true)
	if err == nil {
		m.DiffReport = rep
		if m.DiffCursor >= len(rep.Items) {
			m.DiffCursor = 0
		}
	}
}

// Init implements tea.Model.
func (m *AppModel) Init() tea.Cmd {
	return nil
}

// Update handles keyboard navigation and actions in the TUI.
func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		return m, nil

	case tea.KeyMsg:
		key := msg.String()

		// Handle interactive Freeze input prompt
		if m.InputMode {
			switch key {
			case "esc":
				m.InputMode = false
				m.InputStep = 0
				m.StatusMsg = "Congelado cancelado."
				return m, nil
			case "enter":
				if m.InputStep == 1 {
					m.InputStep = 2
					return m, nil
				}
				m.InputMode = false
				m.InputStep = 0
				m.executeFreeze(strings.TrimSpace(m.InputTag), strings.TrimSpace(m.InputMessage))
				return m, nil
			case "backspace":
				if m.InputStep == 1 && len(m.InputTag) > 0 {
					m.InputTag = m.InputTag[:len(m.InputTag)-1]
				} else if m.InputStep == 2 && len(m.InputMessage) > 0 {
					m.InputMessage = m.InputMessage[:len(m.InputMessage)-1]
				}
				return m, nil
			default:
				if len(key) == 1 {
					if m.InputStep == 1 {
						m.InputTag += key
					} else if m.InputStep == 2 {
						m.InputMessage += key
					}
				}
				return m, nil
			}
		}

		// Global shortcuts
		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "1":
			m.ActiveTab = TabSnapshots
			return m, nil
		case "2":
			m.ActiveTab = TabDiff
			m.refreshDiff()
			return m, nil
		case "3", "c":
			m.ActiveTab = TabFeatures
			return m, nil
		case "4", "a":
			m.ActiveTab = TabAudit
			return m, nil
		case "tab":
			m.ActiveTab = (m.ActiveTab + 1) % 4
			if m.ActiveTab == TabDiff {
				m.refreshDiff()
			}
			return m, nil
		}

		// Tab-specific navigation
		switch m.ActiveTab {
		case TabSnapshots:
			return m.updateSnapshotsTab(key)
		case TabDiff:
			return m.updateDiffTab(key)
		case TabFeatures:
			return m.updateFeaturesTab(key)
		case TabAudit:
			if key == "g" {
				res, err := m.Vault.GarbageCollect()
				if err == nil {
					m.StatusMsg = fmt.Sprintf("♻ GC completado: %d bloques huérfanos eliminados (%d B recuperados)", res.OrphanBlobsFreed, res.BytesReclaimed)
				}
			}
			return m, nil
		}
	}
	return m, nil
}

func (m *AppModel) updateSnapshotsTab(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.SnapCursor > 0 {
			m.SnapCursor--
			m.refreshDiff()
		}
	case "down", "j":
		if m.SnapCursor+1 < len(m.Snapshots) {
			m.SnapCursor++
			m.refreshDiff()
		}
	case "left", "h":
		if m.ProjectCursor > 0 {
			m.ProjectCursor--
			m.SnapCursor = 0
			m.RefreshData()
		}
	case "right", "l":
		if m.ProjectCursor+1 < len(m.Projects) {
			m.ProjectCursor++
			m.SnapCursor = 0
			m.RefreshData()
		}
	case "f":
		m.InputMode = true
		m.InputStep = 1
		m.InputTag = fmt.Sprintf("v%d", len(m.Snapshots)+1)
		m.InputMessage = "Snapshot desde Permafrost TUI"
	case "enter", "s":
		if len(m.Projects) > 0 && len(m.Snapshots) > 0 {
			proj := m.Projects[m.ProjectCursor]
			snap := m.Snapshots[m.SnapCursor]
			m.executeSwitch(proj.Path, snap, nil)
		}
	case "d":
		m.ActiveTab = TabDiff
		m.refreshDiff()
	case "x", "delete":
		if len(m.Projects) > 0 && len(m.Snapshots) > 0 {
			proj := m.Projects[m.ProjectCursor]
			snap := m.Snapshots[m.SnapCursor]
			if _, err := m.Vault.DeleteManifest(proj.ID, snap.ID, false); err == nil {
				m.StatusMsg = fmt.Sprintf("🗑 Snapshot %s (%s) eliminado.", snap.Tag, snap.ID)
				m.RefreshData()
			}
		}
	case "g":
		if res, err := m.Vault.GarbageCollect(); err == nil {
			m.StatusMsg = fmt.Sprintf("♻ GC: %d bloques huérfanos liberados (%d B)", res.OrphanBlobsFreed, res.BytesReclaimed)
			m.RefreshData()
		}
	}
	return m, nil
}

func (m *AppModel) updateDiffTab(key string) (tea.Model, tea.Cmd) {
	if m.DiffReport == nil {
		return m, nil
	}
	switch key {
	case "up", "k":
		if m.DiffCursor > 0 {
			m.DiffCursor--
		}
	case "down", "j":
		if m.DiffCursor+1 < len(m.DiffReport.Items) {
			m.DiffCursor++
		}
	case " ":
		if m.DiffCursor < len(m.DiffReport.Items) {
			p := m.DiffReport.Items[m.DiffCursor].RelPath
			m.SelectedFiles[p] = !m.SelectedFiles[p]
			if !m.SelectedFiles[p] {
				delete(m.SelectedFiles, p)
			}
		}
	case "r", "enter":
		if len(m.Projects) > 0 && len(m.Snapshots) > 0 {
			proj := m.Projects[m.ProjectCursor]
			snap := m.Snapshots[m.SnapCursor]
			var paths []string
			for p, sel := range m.SelectedFiles {
				if sel {
					paths = append(paths, p)
				}
			}
			if len(paths) == 0 && m.DiffCursor < len(m.DiffReport.Items) {
				paths = []string{m.DiffReport.Items[m.DiffCursor].RelPath}
			}
			if len(paths) > 0 {
				m.executeSwitch(proj.Path, snap, paths)
				m.SelectedFiles = make(map[string]bool)
			}
		}
	}
	return m, nil
}

func (m *AppModel) updateFeaturesTab(key string) (tea.Model, tea.Cmd) {
	features := m.Vault.Config.ListFeatures()
	switch key {
	case "up", "k":
		if m.FeatCursor > 0 {
			m.FeatCursor--
		}
	case "down", "j":
		if m.FeatCursor+1 < len(features) {
			m.FeatCursor++
		}
	case " ", "enter":
		if m.FeatCursor < len(features) {
			f := features[m.FeatCursor]
			newState := !f.Enabled
			if err := m.Vault.Config.SetFeature(string(f.Key), newState); err == nil {
				m.Vault.AppendAuditLog("FEATURE_TOGGLED", fmt.Sprintf("%s=%v (via TUI)", f.Key, newState))
				stateLabel := "ACTIVADO [ON]"
				if !newState {
					stateLabel = "DESACTIVADO [OFF]"
				}
				m.StatusMsg = fmt.Sprintf("⚙ Módulo %q %s", f.Title, stateLabel)
			}
		}
	}
	return m, nil
}

func (m *AppModel) executeFreeze(tag, message string) {
	targetPath := m.WorkingDir
	if len(m.Projects) > 0 {
		targetPath = m.Projects[m.ProjectCursor].Path
	}

	if m.OnFreeze != nil {
		snap, extra, err := m.OnFreeze(targetPath, tag, message)
		if err != nil {
			m.StatusMsg = fmt.Sprintf("✖ Error al congelar: %v", err)
			return
		}
		m.StatusMsg = fmt.Sprintf("✔ Congelado %s (%s) — Ahorro %.1f%% %s", snap.Tag, snap.ID, snap.Stats.SpaceSavedPercent, extra)
		m.RefreshData()
		return
	}

	snap, err := m.Engine.Freeze(targetPath, engine.FreezeOptions{
		Tag:     tag,
		Message: message,
	})
	if err != nil {
		m.StatusMsg = fmt.Sprintf("✖ Error al congelar: %v", err)
		return
	}
	m.StatusMsg = fmt.Sprintf("✔ Congelado %s (%s) — %d archivos, ahorro %.1f%%", snap.Tag, snap.ID, snap.Stats.TotalFiles, snap.Stats.SpaceSavedPercent)
	m.RefreshData()
}

func (m *AppModel) executeSwitch(projectPath string, target *vault.Manifest, selectivePaths []string) {
	if m.OnSwitch != nil {
		res, extra, err := m.OnSwitch(projectPath, target, selectivePaths)
		if err != nil {
			m.StatusMsg = fmt.Sprintf("✖ Error al cambiar: %v", err)
			return
		}
		m.StatusMsg = fmt.Sprintf("⚡ Versión %s aplicada (%d actualizados, %d intactos) %s", res.TargetTag, res.FilesUpdated+res.FilesCreated, res.FilesSkippedUnchanged, extra)
		m.RefreshData()
		return
	}

	cleanUntracked := len(selectivePaths) == 0
	res, err := m.Engine.Restore(projectPath, target, selectivePaths, cleanUntracked)
	if err != nil {
		m.StatusMsg = fmt.Sprintf("✖ Error al restaurar: %v", err)
		return
	}
	if len(selectivePaths) > 0 {
		m.StatusMsg = fmt.Sprintf("🍒 Cherry-Pick completado desde %s (%d archivos restaurados)", target.Tag, len(selectivePaths))
	} else {
		m.StatusMsg = fmt.Sprintf("⚡ Cambio a %s completado en %d ms (%d actualizados, %d intactos)", target.Tag, res.DurationMs, res.FilesUpdated+res.FilesCreated, res.FilesSkippedUnchanged)
	}
	m.RefreshData()
}

// View renders the complete visual TUI screen.
func (m *AppModel) View() string {
	var b strings.Builder

	// Top Header Bar
	stats, _ := m.Vault.ComputeVaultStats()
	header := titleStyle.Render(fmt.Sprintf("❄ PERMAFROST (pfrost)  •  Proyectos: %d  •  Snapshots: %d  •  Ahorro Deduplicación: %.1f%%",
		stats.ProjectCount, stats.SnapshotCount, stats.SavingsPercent))
	b.WriteString(header + "\n\n")

	// Navigation Tabs
	tabs := []string{
		"[1] ❄ Versiones & Snapshots",
		"[2] 🔍 Time-Travel Diff & Cherry-Pick",
		"[3] ⚙ Feature Center (ON/OFF)",
		"[4] 🛡 Auditoría & Vault",
	}
	var renderedTabs []string
	for i, t := range tabs {
		if TabIndex(i) == m.ActiveTab {
			renderedTabs = append(renderedTabs, activeTabStyle.Render(t))
		} else {
			renderedTabs = append(renderedTabs, inactiveTabStyle.Render(t))
		}
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...) + "\n\n")

	// Modal prompt for Freeze
	if m.InputMode {
		var prompt strings.Builder
		prompt.WriteString(selectedRowStyle.Render("❄ CONGELAR NUEVA VERSIÓN (SNAPSHOT)") + "\n\n")
		if m.InputStep == 1 {
			prompt.WriteString(fmt.Sprintf("  Tag de la versión: [%s█]\n", m.InputTag))
			prompt.WriteString(dimStyle.Render("  Pulsa Enter para continuar al mensaje, o Esc para cancelar."))
		} else {
			prompt.WriteString(fmt.Sprintf("  Tag de la versión: %s\n", m.InputTag))
			prompt.WriteString(fmt.Sprintf("  Descripción/Nota:  [%s█]\n", m.InputMessage))
			prompt.WriteString(dimStyle.Render("  Pulsa Enter para congelar ahora, o Esc para cancelar."))
		}
		b.WriteString(boxStyle.Render(prompt.String()) + "\n")
		return b.String()
	}

	// Main Content Body
	switch m.ActiveTab {
	case TabSnapshots:
		b.WriteString(m.viewSnapshotsTab())
	case TabDiff:
		b.WriteString(m.viewDiffTab())
	case TabFeatures:
		b.WriteString(m.viewFeaturesTab())
	case TabAudit:
		b.WriteString(m.viewAuditTab(stats))
	}

	// Status bar + Shortcuts footer
	if m.StatusMsg != "" {
		b.WriteString("\n" + statusStyle.Render("ℹ "+m.StatusMsg) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("Atajos: [Tab/1-4] Pestañas • [↑/↓] Navegar • [f] Congelar • [Enter/s] Cambiar Versión • [d] Ver Diff • [c] Módulos ON/OFF • [g] GC • [q] Salir") + "\n")

	return b.String()
}

func (m *AppModel) viewSnapshotsTab() string {
	var b strings.Builder

	if len(m.Projects) == 0 {
		b.WriteString("  No hay proyectos registrados todavía.\n")
		b.WriteString("  Pulsa [f] para congelar la carpeta actual (" + m.WorkingDir + ") o ejecuta 'pfrost freeze'.\n")
		return boxStyle.Render(b.String())
	}

	activeProj := m.Projects[m.ProjectCursor]
	b.WriteString(fmt.Sprintf("Proyecto Activo (%d/%d): %s  %s\n",
		m.ProjectCursor+1, len(m.Projects),
		selectedRowStyle.Render(activeProj.Name),
		dimStyle.Render("("+activeProj.Path+")")))
	b.WriteString(strings.Repeat("─", 88) + "\n")

	if len(m.Snapshots) == 0 {
		b.WriteString("  (Sin versiones congeladas aún. Pulsa [f] para crear el primer snapshot)\n")
		return boxStyle.Render(b.String())
	}

	for i, s := range m.Snapshots {
		cursor := "  "
		if i == m.SnapCursor {
			cursor = "❯ "
		}
		activeBadge := "   "
		if s.ID == activeProj.ActiveSnapshotID {
			activeBadge = greenBadge.Render("[*]")
		}
		line := fmt.Sprintf("%s%s %-12s │ %-26s │ %4d arch │ Nuevo: %-9s (Ahorro %4.1f%%) │ %s │ %s",
			cursor,
			activeBadge,
			s.Tag,
			s.ID,
			s.Stats.TotalFiles,
			formatBytesUI(s.Stats.NewBytesStored),
			s.Stats.SpaceSavedPercent,
			s.CreatedAt.Local().Format("02/01 15:04"),
			s.Message,
		)
		if i == m.SnapCursor {
			b.WriteString(selectedRowStyle.Render(line) + "\n")
		} else {
			b.WriteString(line + "\n")
		}
	}

	// Quick preview of Time-Travel Diff if enabled
	if m.Vault.Config.Features.TimeTravelDiff && m.DiffReport != nil {
		b.WriteString("\n" + strings.Repeat("─", 88) + "\n")
		b.WriteString(fmt.Sprintf("Vista Previa Time-Travel Diff (Carpeta Actual ➔ %s):  %s  %s  %s  (Pulsa [d] para inspeccionar/cherry-pick)\n",
			m.Snapshots[m.SnapCursor].Tag,
			greenBadge.Render(fmt.Sprintf("+%d creados", m.DiffReport.AddedCount)),
			yellowText.Render(fmt.Sprintf("~%d modificados", m.DiffReport.ModifiedCount)),
			redBadge.Render(fmt.Sprintf("-%d eliminados", m.DiffReport.RemovedCount)),
		))
	}

	return boxStyle.Render(b.String())
}

func (m *AppModel) viewDiffTab() string {
	var b strings.Builder
	if m.DiffReport == nil {
		b.WriteString("Selecciona un proyecto con al menos una versión congelada para inspeccionar el Time-Travel Diff.\n")
		return boxStyle.Render(b.String())
	}

	b.WriteString(fmt.Sprintf("Comparando: %s  ➔  %s\n",
		selectedRowStyle.Render(m.DiffReport.SourceLabel),
		selectedRowStyle.Render(m.DiffReport.TargetLabel)))
	b.WriteString(fmt.Sprintf("Resumen: %s  │  %s  │  %s  │  Intactos: %d\n",
		greenBadge.Render(fmt.Sprintf("🟢 +%d creados", m.DiffReport.AddedCount)),
		yellowText.Render(fmt.Sprintf("🟡 ~%d modificados", m.DiffReport.ModifiedCount)),
		redBadge.Render(fmt.Sprintf("🔴 -%d eliminados", m.DiffReport.RemovedCount)),
		m.DiffReport.UnchangedCount))
	b.WriteString(dimStyle.Render("Pulsa [Espacio] para marcar archivos sueltos y [r / Enter] para restaurar SOLO esos archivos (Cherry-Pick).") + "\n")
	b.WriteString(strings.Repeat("─", 88) + "\n")

	for i, item := range m.DiffReport.Items {
		cursor := "  "
		if i == m.DiffCursor {
			cursor = "❯ "
		}
		check := "[ ]"
		if m.SelectedFiles[item.RelPath] {
			check = greenBadge.Render("[✓]")
		}

		var statusTag string
		switch item.Status {
		case diff.StatusAdded:
			statusTag = greenBadge.Render("🟢 + CREADO    ")
		case diff.StatusModified:
			statusTag = yellowText.Render("🟡 ~ MODIFICADO")
		case diff.StatusRemoved:
			statusTag = redBadge.Render("🔴 - ELIMINADO ")
		case diff.StatusUnchanged:
			statusTag = dimStyle.Render("⚪ = INTACTO   ")
		}

		row := fmt.Sprintf("%s%s %s │ %-44s │ %10s",
			cursor, check, statusTag, item.RelPath, formatBytesUI(item.NewSize))
		if i == m.DiffCursor {
			b.WriteString(selectedRowStyle.Render(row) + "\n")
		} else {
			b.WriteString(row + "\n")
		}
	}

	return boxStyle.Render(b.String())
}

func (m *AppModel) viewFeaturesTab() string {
	var b strings.Builder
	b.WriteString(selectedRowStyle.Render("⚙ PANEL DE CONTROL DE MÓDULOS DE PERMAFROST (Transparencia & Control Total)") + "\n")
	b.WriteString(dimStyle.Render("Pulsa [Espacio] o [Enter] sobre cualquier módulo para activarlo [ON] o desactivarlo [OFF] al instante.") + "\n")
	b.WriteString(strings.Repeat("─", 88) + "\n")

	features := m.Vault.Config.ListFeatures()
	for i, f := range features {
		cursor := "  "
		if i == m.FeatCursor {
			cursor = "❯ "
		}
		toggle := redBadge.Render("[OFF]")
		if f.Enabled {
			toggle = greenBadge.Render("[ON ]")
		}
		title := f.Title
		if i == m.FeatCursor {
			title = selectedRowStyle.Render(f.Title)
		}
		b.WriteString(fmt.Sprintf("%s%s  %-36s (%s)\n        %s\n\n",
			cursor, toggle, title, dimStyle.Render(string(f.Key)), f.Description))
	}
	return boxStyle.Render(b.String())
}

func (m *AppModel) viewAuditTab(stats *vault.VaultStats) string {
	var b strings.Builder
	b.WriteString(selectedRowStyle.Render("🛡 BÓVEDA CENTRAL & AUDITORÍA TRANSPARENTE") + "\n")
	b.WriteString(fmt.Sprintf("  • Ruta del Vault:          %s\n", m.Vault.RootDir))
	b.WriteString(fmt.Sprintf("  • Bloques Únicos Activos:  %d bloques deduplicados\n", stats.UniqueBlobCount))
	b.WriteString(fmt.Sprintf("  • Espacio Lógico Total:    %s\n", formatBytesUI(stats.LogicalTrackedBytes)))
	b.WriteString(fmt.Sprintf("  • Espacio Físico Real:     %s (Ahorro: %.1f%%)\n", formatBytesUI(stats.PhysicalVaultBytes), stats.SavingsPercent))
	b.WriteString(strings.Repeat("─", 88) + "\n")
	b.WriteString(selectedRowStyle.Render("Últimos Eventos Registrados (audit.log):") + "\n")

	logs, _ := m.Vault.ReadAuditLogs(12)
	if len(logs) == 0 {
		b.WriteString("  (No hay eventos registrados aún)\n")
	} else {
		for _, l := range logs {
			b.WriteString("  " + dimStyle.Render(l) + "\n")
		}
	}
	return boxStyle.Render(b.String())
}

// RunInteractive launches the full-screen Bubble Tea TUI, falling back to guided line menu if no TTY is available.
func RunInteractive(m *AppModel) error {
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		// Fallback to guided line-by-line interactive CLI menu when running in non-TTY / piped shells
		return RunGuidedMenu(m, os.Stdin, os.Stdout)
	}
	return nil
}

// RunGuidedMenu provides an intuitive numbered interactive menu when raw TTY mode is unavailable.
func RunGuidedMenu(m *AppModel, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		fmt.Fprintln(out, m.View())
		fmt.Fprint(out, "\n\033[36mMenú Guiado Permafrost\033[0m [1-4: Pestañas | f: Congelar | s: Switch | d: Diff | c: Módulos | q: Salir] > ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil
		}
		cmd := strings.TrimSpace(line)
		switch strings.ToLower(cmd) {
		case "q", "quit", "exit":
			return nil
		case "1":
			m.ActiveTab = TabSnapshots
		case "2", "d":
			m.ActiveTab = TabDiff
			m.refreshDiff()
		case "3", "c":
			m.ActiveTab = TabFeatures
		case "4", "a":
			m.ActiveTab = TabAudit
		case "f":
			fmt.Fprint(out, "  Tag de versión (Enter para auto): ")
			tag, _ := reader.ReadString('\n')
			fmt.Fprint(out, "  Mensaje descriptivo: ")
			msg, _ := reader.ReadString('\n')
			m.executeFreeze(strings.TrimSpace(tag), strings.TrimSpace(msg))
		case "s":
			fmt.Fprint(out, "  Tag o ID de versión a restaurar: ")
			ref, _ := reader.ReadString('\n')
			ref = strings.TrimSpace(ref)
			if len(m.Projects) > 0 && ref != "" {
				proj := m.Projects[m.ProjectCursor]
				if snap, err := m.Vault.LoadManifest(proj.ID, ref, true); err == nil {
					m.executeSwitch(proj.Path, snap, nil)
				} else {
					m.StatusMsg = err.Error()
				}
			}
		}
	}
}

func formatBytesUI(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
