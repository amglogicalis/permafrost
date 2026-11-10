package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amglogicalis/permafrost/internal/binaries"
	"github.com/amglogicalis/permafrost/internal/cloud"
	"github.com/amglogicalis/permafrost/internal/crypto"
	"github.com/amglogicalis/permafrost/internal/diff"
	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/hooks"
	"github.com/amglogicalis/permafrost/internal/safeswitch"
	"github.com/amglogicalis/permafrost/internal/ui"
	"github.com/amglogicalis/permafrost/internal/vault"
)

var version = "0.5.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31m✖ Error:\033[0m %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	v, err := vault.Open("")
	if err != nil {
		return err
	}
	eng, err := engine.New(v)
	if err != nil {
		return err
	}
	ssMgr := safeswitch.New(v, eng)
	hookRunner := hooks.New(v)
	binMgr, err := binaries.New(v, eng)
	if err != nil {
		return err
	}
	cloudMgr := cloud.New(v)

	cwd, _ := os.Getwd()

	// Build TUI with Phase 3 SafeSwitch & Hooks wired in
	buildApp := func(targetDir string) *ui.AppModel {
		app := ui.NewAppModel(v, eng, targetDir)
		app.OnFreeze = func(projectPath, tag, message string) (*vault.Manifest, string, error) {
			_, preNotice, err := hookRunner.ExecuteStage(projectPath, hooks.StagePreFreeze, tag)
			if err != nil {
				return nil, "", err
			}
			m, err := eng.Freeze(projectPath, engine.FreezeOptions{
				Tag:     tag,
				Message: message,
			})
			if err != nil {
				return nil, "", err
			}
			_, postNotice, err := hookRunner.ExecuteStage(projectPath, hooks.StagePostFreeze, m.Tag)
			if err != nil {
				return m, "", err
			}
			extra := strings.TrimSpace(preNotice + " " + postNotice)
			return m, extra, nil
		}
		app.OnSwitch = func(projectPath string, target *vault.Manifest, selectivePaths []string) (*engine.SwitchResult, string, error) {
			var notices []string
			if len(selectivePaths) == 0 {
				if proj, pErr := v.FindProject(projectPath); pErr == nil {
					if stash, _, sErr := ssMgr.CheckAndStash(proj, target); sErr == nil && stash != nil {
						notices = append(notices, fmt.Sprintf("[SafeSwitch: %s]", stash.Tag))
					}
				}
				if _, preNotice, hErr := hookRunner.ExecuteStage(projectPath, hooks.StagePreSwitch, target.Tag); hErr != nil {
					return nil, "", hErr
				} else if preNotice != "" {
					notices = append(notices, preNotice)
				}
			}
			res, err := eng.Restore(projectPath, target, selectivePaths, len(selectivePaths) == 0)
			if err != nil {
				return nil, "", err
			}
			if len(selectivePaths) == 0 {
				if _, postNotice, hErr := hookRunner.ExecuteStage(projectPath, hooks.StagePostSwitch, target.Tag); hErr != nil {
					return res, "", hErr
				} else if postNotice != "" {
					notices = append(notices, postNotice)
				}
			}
			return res, strings.Join(notices, " "), nil
		}
		return app
	}

	if len(args) == 0 {
		return ui.RunInteractive(buildApp(cwd))
	}

	cmd := strings.ToLower(args[0])
	rest := args[1:]

	switch cmd {
	case "ui", "tui", "menu":
		targetDir := cwd
		if len(rest) > 0 {
			targetDir = rest[0]
		}
		return ui.RunInteractive(buildApp(targetDir))

	case "dashboard":
		targetDir := cwd
		tab := ui.TabSnapshots
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--diff":
				tab = ui.TabDiff
			case "--features":
				tab = ui.TabFeatures
			case "--audit":
				tab = ui.TabAudit
			default:
				if !strings.HasPrefix(rest[i], "-") {
					targetDir = rest[i]
				}
			}
		}
		app := buildApp(targetDir)
		app.ActiveTab = tab
		fmt.Println(app.View())
		return nil

	case "version", "-v", "--version":
		fmt.Printf("pfrost v%s (Vault: %s)\n", version, v.RootDir)
		return nil

	case "help", "-h", "--help":
		printBanner(v)
		printHelp()
		return nil

	case "init":
		targetDir := cwd
		name := ""
		if len(rest) > 0 {
			targetDir = rest[0]
		}
		if len(rest) > 1 {
			name = rest[1]
		}
		proj, err := v.RegisterOrGetProject(targetDir, name)
		if err != nil {
			return err
		}
		fmt.Printf("\033[36m❄ Proyecto registrado en Permafrost:\033[0m\n")
		fmt.Printf("  • Nombre:     \033[1m%s\033[0m\n", proj.Name)
		fmt.Printf("  • ID:         %s\n", proj.ID)
		fmt.Printf("  • Ruta:       %s\n", proj.Path)
		fmt.Printf("  • Vault:      %s\n", v.RootDir)
		return nil

	case "freeze", "snap", "save":
		targetDir := cwd
		tag := ""
		msg := "Snapshot creado desde pfrost CLI"
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "-t", "--tag":
				if i+1 < len(rest) {
					tag = rest[i+1]
					i++
				}
			case "-m", "--message":
				if i+1 < len(rest) {
					msg = rest[i+1]
					i++
				}
			case "-p", "--path":
				if i+1 < len(rest) {
					targetDir = rest[i+1]
					i++
				}
			default:
				if tag == "" && !strings.HasPrefix(rest[i], "-") {
					tag = rest[i]
				} else if msg == "Snapshot creado desde pfrost CLI" && !strings.HasPrefix(rest[i], "-") {
					msg = rest[i]
				}
			}
		}

		// Pre-freeze hooks
		printHookResults(hookRunner.ExecuteStage(targetDir, hooks.StagePreFreeze, tag))

		m, err := eng.Freeze(targetDir, engine.FreezeOptions{
			Tag:     tag,
			Message: msg,
		})
		if err != nil {
			return err
		}

		fmt.Printf("\033[32m✔ Versión congelada con éxito:\033[0m \033[1m%s\033[0m (%s)\n", m.Tag, m.ID)
		fmt.Printf("  • Proyecto:            %s (%s)\n", m.ProjectName, m.RootPath)
		fmt.Printf("  • Archivos rastreados: %d (%s lógicos)\n", m.Stats.TotalFiles, formatBytes(m.Stats.LogicalBytes))
		fmt.Printf("  • Bloques FastCDC:     %d totales (%d nuevos, %d reutilizados por deduplicación)\n",
			m.Stats.TotalChunks, m.Stats.NewUniqueChunks, m.Stats.ReusedChunks)
		fmt.Printf("  • Espacio nuevo disco: \033[36m%s\033[0m (Ahorro: \033[32m%.1f%%\033[0m)\n",
			formatBytes(m.Stats.NewBytesStored), m.Stats.SpaceSavedPercent)
		fmt.Printf("  • Tiempo:              %d ms\n", m.Stats.DurationMs)

		// Post-freeze hooks
		printHookResults(hookRunner.ExecuteStage(targetDir, hooks.StagePostFreeze, m.Tag))
		return nil

	case "switch", "restore", "checkout":
		if len(rest) == 0 {
			return fmt.Errorf("especifica el tag o ID de la versión a restaurar (ej: pfrost switch v1)")
		}
		targetRef := rest[0]
		targetDir := cwd
		skipSafeSwitch := false
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case "-p", "--path":
				if i+1 < len(rest) {
					targetDir = rest[i+1]
					i++
				}
			case "--no-safe-switch", "--force", "-f":
				skipSafeSwitch = true
			}
		}

		proj, err := v.FindProject(targetDir)
		if err != nil {
			return err
		}
		m, err := v.LoadManifest(proj.ID, targetRef, true)
		if err != nil {
			return err
		}

		// 1. Safe Switch check
		if !skipSafeSwitch {
			if ssMgr.IsEnabled() {
				stash, rep, sErr := ssMgr.CheckAndStash(proj, m)
				if sErr != nil {
					return sErr
				}
				if stash != nil {
					fmt.Printf("\033[36m🛡 Safe Switch [ON]:\033[0m Detectados cambios sin congelar (+%d ~%d -%d). Auto-stash creado: \033[1m%s\033[0m (%s)\n",
						rep.AddedCount, rep.ModifiedCount, rep.RemovedCount, stash.Tag, stash.ID)
				} else {
					fmt.Printf("\033[36m🛡 Safe Switch [ON]:\033[0m Carpeta limpia respecto a la versión activa (no se requirió auto-stash).\n")
				}
			} else {
				fmt.Printf("\033[33mℹ Safe Switch [OFF]:\033[0m Cambiando de versión sin auto-stash previo (puedes activarlo con 'pfrost safeswitch on').\n")
			}
		}

		// 2. Pre-switch hooks
		if err := printHookResults(hookRunner.ExecuteStage(proj.Path, hooks.StagePreSwitch, m.Tag)); err != nil {
			return err
		}

		// 3. Show Time-Travel Diff summary if enabled
		if v.Config.Features.TimeTravelDiff {
			if rep, dErr := diff.CompareWorkingTreeToSnapshot(eng, proj.Path, m, true); dErr == nil {
				fmt.Print(rep.FormatColored(false))
				fmt.Println()
			}
		}

		// 4. Hot-Swap Restore
		res, err := eng.Restore(proj.Path, m, nil, true)
		if err != nil {
			return err
		}

		fmt.Printf("\033[32m⚡ Cambio de versión completado:\033[0m \033[1m%s\033[0m (%s)\n", res.TargetTag, res.TargetSnapshotID)
		fmt.Printf("  • Archivos creados:     %d\n", res.FilesCreated)
		fmt.Printf("  • Archivos modificados: %d\n", res.FilesUpdated)
		fmt.Printf("  • Intactos (omitidos):  %d (0 I/O gracias a SHA-256 fast-skip)\n", res.FilesSkippedUnchanged)
		fmt.Printf("  • Archivos limpiados:   %d\n", res.FilesRemoved)
		fmt.Printf("  • Datos reconstruidos:  %s en %d ms\n", formatBytes(res.BytesWritten), res.DurationMs)

		// 5. Post-switch hooks
		return printHookResults(hookRunner.ExecuteStage(proj.Path, hooks.StagePostSwitch, m.Tag))

	case "safeswitch", "stash":
		return handleSafeSwitchCmd(v, ssMgr, cwd, rest)

	case "hooks", "hook":
		return handleHooksCmd(v, hookRunner, cwd, rest)

	case "bin", "binaries":
		return handleBinariesCmd(v, binMgr, rest)

	case "crypto", "encryption":
		return handleCryptoCmd(v, rest)

	case "cloud", "sync":
		return handleCloudCmd(v, eng, cloudMgr, cwd, rest)

	case "teleport", "share":
		return handleTeleportCmd(v, eng, cloudMgr, cwd, rest)

	case "diff":
		targetDir := cwd
		showAll := false
		var refs []string
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "-p", "--path":
				if i+1 < len(rest) {
					targetDir = rest[i+1]
					i++
				}
			case "-a", "--all":
				showAll = true
			default:
				if !strings.HasPrefix(rest[i], "-") {
					refs = append(refs, rest[i])
				}
			}
		}

		proj, err := v.FindProject(targetDir)
		if err != nil {
			return err
		}

		var rep *diff.Report
		switch len(refs) {
		case 0:
			ref := proj.ActiveSnapshotID
			if ref == "" {
				snaps, _ := v.ListManifests(proj.ID, false)
				if len(snaps) == 0 {
					return fmt.Errorf("no hay snapshots en el proyecto %s para comparar", proj.Name)
				}
				ref = snaps[0].ID
			}
			m, err := v.LoadManifest(proj.ID, ref, true)
			if err != nil {
				return err
			}
			rep, err = diff.CompareWorkingTreeToSnapshot(eng, proj.Path, m, false)
			if err != nil {
				return err
			}
		case 1:
			m, err := v.LoadManifest(proj.ID, refs[0], true)
			if err != nil {
				return err
			}
			rep, err = diff.CompareWorkingTreeToSnapshot(eng, proj.Path, m, true)
			if err != nil {
				return err
			}
		default:
			m1, err := v.LoadManifest(proj.ID, refs[0], true)
			if err != nil {
				return err
			}
			m2, err := v.LoadManifest(proj.ID, refs[1], true)
			if err != nil {
				return err
			}
			rep = diff.CompareEntries(
				fmt.Sprintf("%s (%s)", m1.Tag, m1.ID), m1.Files,
				fmt.Sprintf("%s (%s)", m2.Tag, m2.ID), m2.Files,
			)
		}

		fmt.Print(rep.FormatColored(showAll))
		return nil

	case "pick", "cherry-pick":
		if len(rest) < 2 {
			return fmt.Errorf("uso: pfrost pick <tag|id> <archivo1> [archivo2...] [-p ruta]")
		}
		targetRef := rest[0]
		targetDir := cwd
		var files []string
		for i := 1; i < len(rest); i++ {
			if (rest[i] == "-p" || rest[i] == "--path") && i+1 < len(rest) {
				targetDir = rest[i+1]
				i++
			} else if !strings.HasPrefix(rest[i], "-") {
				files = append(files, rest[i])
			}
		}
		if len(files) == 0 {
			return fmt.Errorf("especifica al menos un archivo o subcarpeta para restaurar selectivamente")
		}

		proj, err := v.FindProject(targetDir)
		if err != nil {
			return err
		}
		m, err := v.LoadManifest(proj.ID, targetRef, true)
		if err != nil {
			return err
		}

		res, err := eng.Restore(proj.Path, m, files, false)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m🍒 Restauración selectiva (Cherry-Pick) completada desde %s:\033[0m\n", m.Tag)
		fmt.Printf("  • Archivos solicitados: %s\n", strings.Join(files, ", "))
		fmt.Printf("  • Creados/Actualizados: %d (%d omitidos por ser idénticos)\n",
			res.FilesCreated+res.FilesUpdated, res.FilesSkippedUnchanged)
		fmt.Printf("  • Datos escritos:       %s en %d ms\n", formatBytes(res.BytesWritten), res.DurationMs)
		return nil

	case "list", "ls":
		targetDir := cwd
		if len(rest) > 0 {
			targetDir = rest[0]
		}
		proj, err := v.FindProject(targetDir)
		if err != nil {
			return err
		}
		snaps, err := v.ListManifests(proj.ID, false)
		if err != nil {
			return err
		}
		fmt.Printf("\033[36m❄ Versiones de %s\033[0m (%s):\n", proj.Name, proj.Path)
		if len(snaps) == 0 {
			fmt.Println("  (No hay versiones congeladas aún. Usa 'pfrost freeze <tag>')")
			return nil
		}
		for _, s := range snaps {
			activeMark := "  "
			if s.ID == proj.ActiveSnapshotID {
				activeMark = "\033[32m* \033[0m"
			}
			fmt.Printf("%s\033[1m%-14s\033[0m | %-28s | %6d files | %10s lógicos -> %10s nuevos | %s | %s\n",
				activeMark,
				s.Tag,
				s.ID,
				s.Stats.TotalFiles,
				formatBytes(s.Stats.LogicalBytes),
				formatBytes(s.Stats.NewBytesStored),
				s.CreatedAt.Local().Format("2006-01-02 15:04:05"),
				s.Message,
			)
		}
		return nil

	case "status", "stats":
		st, err := v.ComputeVaultStats()
		if err != nil {
			return err
		}
		printBanner(v)
		fmt.Printf("\033[1mEstado Global del Vault (%s):\033[0m\n", v.RootDir)
		fmt.Printf("  • Proyectos registrados:   %d\n", st.ProjectCount)
		fmt.Printf("  • Versiones congeladas:    %d (+ %d auto-stashes)\n", st.SnapshotCount, st.AutoStashCount)
		fmt.Printf("  • Bloques únicos (CDC):    %d\n", st.UniqueBlobCount)
		fmt.Printf("  • Tamaño lógico total:     %s\n", formatBytes(st.LogicalTrackedBytes))
		fmt.Printf("  • Tamaño físico en Vault:  \033[36m%s\033[0m\n", formatBytes(st.PhysicalVaultBytes))
		fmt.Printf("  • Ahorro deduplicación:    \033[32m%.1f%%\033[0m\n\n", st.SavingsPercent)

		projects := v.ListProjects()
		if len(projects) > 0 {
			fmt.Println("\033[1mProyectos:\033[0m")
			for _, p := range projects {
				fmt.Printf("  • %-18s | Activa: %-10s | Snapshots: %-3d | %s\n",
					p.Name, emptyFallback(p.ActiveTag, "-"), p.SnapshotCount, filepath.Clean(p.Path))
			}
		}
		return nil

	case "gc":
		res, err := v.GarbageCollect()
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m♻ Recolección de basura completada:\033[0m\n")
		fmt.Printf("  • Bloques escaneados:    %d\n", res.ScannedBlobs)
		fmt.Printf("  • Bloques en uso activo: %d\n", res.ReferencedBlobs)
		fmt.Printf("  • Bloques huérfanos:     %d eliminados\n", res.OrphanBlobsFreed)
		fmt.Printf("  • Espacio recuperado:    \033[36m%s\033[0m\n", formatBytes(res.BytesReclaimed))
		return nil

	case "features", "feature", "config":
		if len(rest) >= 2 {
			featName := rest[0]
			stateStr := strings.ToLower(rest[1])
			enable := stateStr == "on" || stateStr == "true" || stateStr == "enable" || stateStr == "1"
			if !enable && stateStr != "off" && stateStr != "false" && stateStr != "disable" && stateStr != "0" {
				return fmt.Errorf("usa 'on' o 'off' para activar o desactivar un módulo (ej: pfrost feature safe_switch on)")
			}
			if err := v.Config.SetFeature(featName, enable); err != nil {
				return err
			}
			v.AppendAuditLog("FEATURE_TOGGLED", fmt.Sprintf("%s=%v", featName, enable))
		}
		fmt.Println("\033[36m❄ Estado de Módulos de Permafrost (Activables / Desactivables):\033[0m")
		for _, f := range v.Config.ListFeatures() {
			badge := "\033[32m[ON ]\033[0m"
			if !f.Enabled {
				badge = "\033[31m[OFF]\033[0m"
			}
			fmt.Printf("  %s \033[1m%-32s\033[0m (%s)\n        %s\n", badge, f.Title, f.Key, f.Description)
		}
		return nil

	case "audit", "logs":
		lines, err := v.ReadAuditLogs(25)
		if err != nil {
			return err
		}
		fmt.Printf("\033[36m❄ Registro Transparente de Auditoría (%s/audit.log):\033[0m\n", v.RootDir)
		for _, l := range lines {
			fmt.Println("  " + l)
		}
		return nil

	default:
		return fmt.Errorf("comando desconocido %q (ejecuta 'pfrost help' para ver los comandos disponibles)", cmd)
	}
}

func handleSafeSwitchCmd(v *vault.Vault, ssMgr *safeswitch.Manager, cwd string, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}

	switch sub {
	case "on", "enable":
		if err := v.Config.SetFeature("safe_switch", true); err != nil {
			return err
		}
		v.AppendAuditLog("SAFE_SWITCH_TOGGLED", "enabled=true")
		fmt.Println("\033[32m✔ Safe Switch ACTIVADO [ON]\033[0m — Se crearán auto-stashes automáticos si hay cambios sin congelar antes de un switch.")
		return nil

	case "off", "disable":
		if err := v.Config.SetFeature("safe_switch", false); err != nil {
			return err
		}
		v.AppendAuditLog("SAFE_SWITCH_TOGGLED", "enabled=false")
		fmt.Println("\033[33mℹ Safe Switch DESACTIVADO [OFF]\033[0m — No se crearán auto-stashes automáticos antes de un switch.")
		return nil

	case "status", "list", "ls":
		badge := "\033[32m[ON ]\033[0m"
		if !ssMgr.IsEnabled() {
			badge = "\033[31m[OFF]\033[0m"
		}
		fmt.Printf("\033[36m🛡 Módulo Safe Switch:\033[0m %s (Máx auto-stashes por proyecto: %d)\n", badge, v.Config.Chunking.MaxAutoStashes)

		targetDir := cwd
		if len(args) > 1 {
			targetDir = args[1]
		}
		proj, err := v.FindProject(targetDir)
		if err != nil {
			fmt.Println("  (Directorio actual no registrado aún)")
			return nil
		}
		stashes, _ := ssMgr.ListStashes(proj.ID)
		fmt.Printf("  Auto-Stashes guardados en \033[1m%s\033[0m (%d):\n", proj.Name, len(stashes))
		for _, s := range stashes {
			fmt.Printf("    • \033[1m%-24s\033[0m | %-28s | %4d arch | %s | %s\n",
				s.Tag, s.ID, s.Stats.TotalFiles, s.CreatedAt.Local().Format("2006-01-02 15:04:05"), s.Message)
		}
		return nil

	case "restore", "pop":
		if len(args) < 2 {
			return fmt.Errorf("uso: pfrost safeswitch restore <stash-id|tag>")
		}
		proj, err := v.FindProject(cwd)
		if err != nil {
			return err
		}
		drop := sub == "pop"
		res, stash, err := ssMgr.RestoreStash(proj, args[1], drop)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Auto-Stash %s (%s) restaurado con éxito\033[0m (%d archivos actualizados)\n",
			stash.Tag, stash.ID, res.FilesUpdated+res.FilesCreated)
		return nil

	case "promote":
		if len(args) < 3 {
			return fmt.Errorf("uso: pfrost safeswitch promote <stash-id|tag> <nuevo-tag> [mensaje]")
		}
		proj, err := v.FindProject(cwd)
		if err != nil {
			return err
		}
		msg := "Promocionado desde Safe Switch"
		if len(args) > 3 {
			msg = strings.Join(args[3:], " ")
		}
		snap, err := ssMgr.PromoteStash(proj, args[1], args[2], msg)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Auto-Stash convertido en versión permanente:\033[0m \033[1m%s\033[0m (%s)\n", snap.Tag, snap.ID)
		return nil

	case "clear":
		proj, err := v.FindProject(cwd)
		if err != nil {
			return err
		}
		n, err := ssMgr.ClearStashes(proj.ID)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Eliminados %d auto-stashes de %s.\033[0m\n", n, proj.Name)
		return nil

	default:
		return fmt.Errorf("subcomando de safeswitch desconocido %q (válidos: status, list, on, off, restore, promote, clear)", sub)
	}
}

func handleHooksCmd(v *vault.Vault, runner *hooks.Runner, cwd string, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}

	switch sub {
	case "on", "enable":
		if err := v.Config.SetFeature("hooks", true); err != nil {
			return err
		}
		v.AppendAuditLog("HOOKS_TOGGLED", "enabled=true")
		fmt.Println("\033[32m✔ Módulo Hooks ACTIVADO [ON]\033[0m")
		return nil

	case "off", "disable":
		if err := v.Config.SetFeature("hooks", false); err != nil {
			return err
		}
		v.AppendAuditLog("HOOKS_TOGGLED", "enabled=false")
		fmt.Println("\033[33mℹ Módulo Hooks DESACTIVADO [OFF]\033[0m")
		return nil

	case "init":
		p, err := runner.InitTemplate(cwd, false)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Plantilla .permafrost.yaml creada y autorizada:\033[0m %s\n", p)
		return nil

	case "trust":
		cfg, err := runner.Trust(cwd)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Archivo .permafrost.yaml autorizado:\033[0m SHA-256 %s\n", cfg.FileHash[:16])
		return nil

	case "revoke":
		if err := runner.Revoke(cwd); err != nil {
			return err
		}
		fmt.Println("\033[33mℹ Autorización revocada para .permafrost.yaml en este proyecto.\033[0m")
		return nil

	case "run", "test":
		if len(args) < 2 {
			return fmt.Errorf("uso: pfrost hooks run <pre_freeze|post_freeze|pre_switch|post_switch>")
		}
		return printHookResults(runner.ExecuteStage(cwd, hooks.Stage(args[1]), "test-tag"))

	case "status":
		badge := "\033[32m[ON ]\033[0m"
		if !runner.IsGloballyEnabled() {
			badge = "\033[31m[OFF]\033[0m"
		}
		fmt.Printf("\033[36m⚡ Módulo Lifecycle Hooks:\033[0m %s\n", badge)
		cfg, err := runner.LoadProjectHooks(cwd)
		if err != nil {
			return err
		}
		if cfg == nil {
			fmt.Printf("  • Archivo en proyecto actual: No encontrado (ejecuta 'pfrost hooks init' para crear .permafrost.yaml)\n")
			return nil
		}
		trustedBadge := "\033[31m[NO AUTORIZADO - ejecuta 'pfrost hooks trust']\033[0m"
		if runner.IsTrusted(cwd, cfg) {
			trustedBadge = "\033[32m[AUTORIZADO ✔]\033[0m"
		}
		fmt.Printf("  • Archivo:       %s\n", cfg.FilePath)
		fmt.Printf("  • Firma SHA-256: %s %s\n", cfg.FileHash[:16], trustedBadge)
		fmt.Printf("  • pre_freeze:    %d comando(s) %v\n", len(cfg.PreFreeze), cfg.PreFreeze)
		fmt.Printf("  • post_freeze:   %d comando(s) %v\n", len(cfg.PostFreeze), cfg.PostFreeze)
		fmt.Printf("  • pre_switch:    %d comando(s) %v\n", len(cfg.PreSwitch), cfg.PreSwitch)
		fmt.Printf("  • post_switch:   %d comando(s) %v\n", len(cfg.PostSwitch), cfg.PostSwitch)
		return nil

	default:
		return fmt.Errorf("subcomando de hooks desconocido %q (válidos: status, init, trust, revoke, on, off, run)", sub)
	}
}

func handleBinariesCmd(v *vault.Vault, bm *binaries.Manager, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}

	switch sub {
	case "on", "enable":
		if err := v.Config.SetFeature("global_binaries", true); err != nil {
			return err
		}
		v.AppendAuditLog("GLOBAL_BINARIES_TOGGLED", "enabled=true")
		fmt.Println("\033[32m✔ Módulo Global Binaries ACTIVADO [ON]\033[0m")
		return nil

	case "off", "disable":
		if err := v.Config.SetFeature("global_binaries", false); err != nil {
			return err
		}
		v.AppendAuditLog("GLOBAL_BINARIES_TOGGLED", "enabled=false")
		fmt.Println("\033[33mℹ Módulo Global Binaries DESACTIVADO [OFF]\033[0m")
		return nil

	case "add", "register":
		if len(args) < 4 {
			return fmt.Errorf("uso: pfrost bin add <nombre-tool> <ruta-ejecutable> <version-tag> [nota]")
		}
		note := "Registrado en Permafrost Global Binaries"
		if len(args) > 4 {
			note = strings.Join(args[4:], " ")
		}
		tv, err := bm.AddBinary(args[1], args[2], args[3], note)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Binario global congelado y activado:\033[0m \033[1m%s %s\033[0m\n", args[1], tv.VersionTag)
		fmt.Printf("  • Ejecutable activo: %s/%s\n", bm.BinDir(), tv.ExecutableName)
		fmt.Printf("  • Tamaño lógico:     %s (Nuevo almacenado con FastCDC: %s)\n",
			formatBytes(tv.LogicalBytes), formatBytes(tv.NewBytesStored))
		return nil

	case "use", "switch":
		if len(args) < 3 {
			return fmt.Errorf("uso: pfrost bin use <nombre-tool> <version-tag>")
		}
		activePath, err := bm.UseVersion(args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m⚡ Herramienta global %s cambiada a la versión %s:\033[0m %s\n", args[1], args[2], activePath)
		return nil

	case "remove", "rm":
		if len(args) < 3 {
			return fmt.Errorf("uso: pfrost bin remove <nombre-tool> <version-tag>")
		}
		if err := bm.RemoveVersion(args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Versión %s de %s eliminada.\033[0m\n", args[2], args[1])
		return nil

	case "path":
		fmt.Printf("\033[36m❄ Directorio de Binarios Globales de Permafrost:\033[0m %s\n\n", bm.BinDir())
		fmt.Println("Para añadirlo a tu PATH:")
		fmt.Printf("  • PowerShell (Windows):  $env:Path = \"$env:USERPROFILE\\.permafrost\\bin;$env:Path\"\n")
		fmt.Printf("  • Bash / Zsh (Linux/Mac): export PATH=\"$HOME/.permafrost/bin:$PATH\"\n")
		return nil

	case "status", "list", "ls":
		badge := "\033[32m[ON ]\033[0m"
		if !bm.IsEnabled() {
			badge = "\033[31m[OFF]\033[0m"
		}
		fmt.Printf("\033[36m🧰 Permafrost Global Binaries:\033[0m %s (Directorio: %s)\n", badge, bm.BinDir())
		tools := bm.ListTools()
		if len(tools) == 0 {
			fmt.Println("  (No hay binarios registrados aún. Usa 'pfrost bin add <tool> <archivo> <version>')")
			return nil
		}
		for _, t := range tools {
			fmt.Printf("\n  \033[1m%s\033[0m (Activa: \033[32m%s\033[0m):\n", t.Name, emptyFallback(t.ActiveVersion, "ninguna"))
			var vKeys []string
			for k := range t.Versions {
				vKeys = append(vKeys, k)
			}
			sort.Strings(vKeys)
			for _, k := range vKeys {
				tv := t.Versions[k]
				mark := "  "
				if k == t.ActiveVersion {
					mark = "\033[32m* \033[0m"
				}
				fmt.Printf("    %s%-12s | %10s lógicos (%10s nuevos) | %s\n",
					mark, tv.VersionTag, formatBytes(tv.LogicalBytes), formatBytes(tv.NewBytesStored), tv.Note)
			}
		}
		return nil

	default:
		return fmt.Errorf("subcomando de bin desconocido %q (válidos: status, list, add, use, remove, path, on, off)", sub)
	}
}

func handleCryptoCmd(v *vault.Vault, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}

	switch sub {
	case "on", "enable":
		if err := v.Config.SetFeature("zero_knowledge_encryption", true); err != nil {
			return err
		}
		v.AppendAuditLog("CRYPTO_TOGGLED", "enabled=true")
		fmt.Println("\033[32m🔒 Encriptación Zero-Knowledge ACTIVADA [ON]\033[0m (AES-256-GCM + PBKDF2-SHA256)")
		return nil

	case "off", "disable":
		if err := v.Config.SetFeature("zero_knowledge_encryption", false); err != nil {
			return err
		}
		v.AppendAuditLog("CRYPTO_TOGGLED", "enabled=false")
		fmt.Println("\033[33m🔓 Encriptación Zero-Knowledge DESACTIVADA [OFF]\033[0m")
		return nil

	case "keygen", "init":
		key, err := crypto.GenerateRecoveryKey()
		if err != nil {
			return err
		}
		saltHex, checkHex, err := crypto.ComputeKeyVerification(key)
		if err != nil {
			return err
		}
		v.Config.Cloud.KeySaltHex = saltHex
		v.Config.Cloud.KeyCheckHex = checkHex
		if err := v.Config.Save(); err != nil {
			return err
		}
		v.AppendAuditLog("CRYPTO_KEY_GENERATED", fmt.Sprintf("Check=%s", checkHex[:8]))
		fmt.Println("\033[32m🔑 Nueva Recovery Key Zero-Knowledge generada con éxito:\033[0m")
		fmt.Printf("\n    \033[1m\033[36m%s\033[0m\n\n", key)
		fmt.Println("  ⚠ Guarda esta clave en un lugar seguro. Permafrost es Zero-Knowledge: nunca guarda tu clave en disco.")
		fmt.Println("  Puedes usarla pasando '--key <clave>' o con la variable de entorno PFROST_KEY.")
		return nil

	case "inspect", "verify":
		if len(args) < 2 {
			return fmt.Errorf("uso: pfrost crypto inspect <archivo>")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		meta := crypto.Inspect(data)
		badge := "\033[33m🔓 SIN CIFRAR\033[0m"
		if meta.Encrypted {
			badge = "\033[32m🔒 CIFRADO ZERO-KNOWLEDGE (PFZK01)\033[0m"
		}
		fmt.Printf("Inspección de Paquete (%s):\n", args[1])
		fmt.Printf("  • Estado:    %s\n", badge)
		fmt.Printf("  • Algoritmo: %s\n", meta.Algorithm)
		fmt.Printf("  • Tamaño:    %s\n", formatBytes(int64(meta.PayloadLen)))
		if meta.SaltHex != "" {
			fmt.Printf("  • Salt Hex:  %s\n", meta.SaltHex)
		}
		return nil

	case "status":
		badge := "\033[32m[ON ]\033[0m 🔒"
		if !v.Config.Features.ZeroKnowledgeEncryption {
			badge = "\033[31m[OFF]\033[0m 🔓"
		}
		fmt.Printf("\033[36m🔐 Módulo Zero-Knowledge Cloud Encryption:\033[0m %s\n", badge)
		fmt.Println("  • Algoritmo:          AES-256-GCM + PBKDF2-HMAC-SHA256 (100,000 iteraciones)")
		fmt.Println("  • Cabecera Mágica:    PFZK01 (Autenticada vía GCM AAD)")
		if v.Config.Cloud.KeyCheckHex != "" {
			fmt.Printf("  • Huella Verificación: %s (Configurada ✔)\n", v.Config.Cloud.KeyCheckHex)
		} else {
			fmt.Println("  • Huella Verificación: No inicializada (usa 'pfrost crypto keygen' para generar una Recovery Key)")
		}
		return nil

	default:
		return fmt.Errorf("subcomando de crypto desconocido %q (válidos: status, on, off, keygen, inspect)", sub)
	}
}

func handleCloudCmd(v *vault.Vault, eng *engine.Engine, cm *cloud.Manager, cwd string, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}

	switch sub {
	case "setup":
		if len(args) < 3 {
			return fmt.Errorf("uso: pfrost cloud setup <github|s3|folder> <repo|endpoint|ruta> [bucket] [region]")
		}
		provType := strings.ToLower(args[1])
		switch provType {
		case "github", "gh":
			v.Config.Cloud.ActiveProvider = "github"
			v.Config.Cloud.GitHubRepo = args[2]
			if err := v.Config.Save(); err != nil {
				return err
			}
			v.AppendAuditLog("CLOUD_CONFIGURED", fmt.Sprintf("Provider=github Repo=%s", args[2]))
			fmt.Printf("\033[32m✔ Backend Cloud configurado:\033[0m GitHub Releases (%s) — Límite por fragmento: %d MB\n",
				args[2], v.Config.Cloud.ChunkLimitMB)
			return nil

		case "s3", "r2":
			if len(args) < 4 {
				return fmt.Errorf("uso: pfrost cloud setup s3 <endpoint-url> <bucket> [region]")
			}
			region := "auto"
			if len(args) >= 5 {
				region = args[4]
			}
			v.Config.Cloud.ActiveProvider = "s3"
			v.Config.Cloud.S3Endpoint = args[2]
			v.Config.Cloud.S3Bucket = args[3]
			v.Config.Cloud.S3Region = region
			if err := v.Config.Save(); err != nil {
				return err
			}
			v.AppendAuditLog("CLOUD_CONFIGURED", fmt.Sprintf("Provider=s3 Endpoint=%s Bucket=%s", args[2], args[3]))
			fmt.Printf("\033[32m✔ Backend Cloud configurado:\033[0m S3/R2 (%s/%s)\n", args[2], args[3])
			return nil

		case "folder", "local":
			abs, err := filepath.Abs(args[2])
			if err != nil {
				return err
			}
			v.Config.Cloud.ActiveProvider = "folder"
			v.Config.Cloud.S3Endpoint = abs
			if err := v.Config.Save(); err != nil {
				return err
			}
			v.AppendAuditLog("CLOUD_CONFIGURED", fmt.Sprintf("Provider=folder Path=%s", abs))
			fmt.Printf("\033[32m✔ Backend Cloud configurado:\033[0m Folder Sync (%s)\n", abs)
			return nil

		default:
			return fmt.Errorf("proveedor desconocido %q (usa: github, s3, folder)", provType)
		}

	case "push":
		prov, err := resolveCloudProvider(v)
		if err != nil {
			return err
		}
		proj, err := v.FindProject(cwd)
		if err != nil {
			return err
		}
		targetRef := proj.ActiveSnapshotID
		key := ""
		for i := 1; i < len(args); i++ {
			if (args[i] == "--key" || args[i] == "-k") && i+1 < len(args) {
				key = args[i+1]
				i++
			} else if !strings.HasPrefix(args[i], "-") {
				targetRef = args[i]
			}
		}
		key = crypto.ResolveKey(key)
		if v.Config.Features.ZeroKnowledgeEncryption && key == "" {
			return fmt.Errorf("el cifrado Zero-Knowledge está [ON]: proporciona tu clave con '--key <clave>' o PFROST_KEY (o desactívalo con 'pfrost crypto off')")
		}
		manifest, err := v.LoadManifest(proj.ID, targetRef, false)
		if err != nil {
			return err
		}
		idx, chunks, err := cm.PackToChunks(manifest, key, 0)
		if err != nil {
			return err
		}
		if err := prov.UploadRelease(idx, chunks); err != nil {
			return err
		}
		v.AppendAuditLog("CLOUD_PUSH", fmt.Sprintf("Provider=%s Release=%s Chunks=%d Encrypted=%v",
			prov.Name(), idx.ReleaseKey, len(chunks), idx.Encrypted))
		fmt.Printf("\033[32m☁ Snapshot subido con éxito a %s:\033[0m\n", prov.Name())
		fmt.Printf("  • Release Key: \033[1m%s\033[0m\n", idx.ReleaseKey)
		fmt.Printf("  • Fragmentos:  %d chunk(s) (%s totales)\n", len(chunks), formatBytes(idx.TotalBytes))
		fmt.Printf("  • Cifrado ZK:  %v\n", idx.Encrypted)
		return nil

	case "pull":
		if len(args) < 2 {
			return fmt.Errorf("uso: pfrost cloud pull <release-key> [--key <clave>] [-p ruta-destino]")
		}
		prov, err := resolveCloudProvider(v)
		if err != nil {
			return err
		}
		relKey := args[1]
		key := ""
		destDir := cwd
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--key", "-k":
				if i+1 < len(args) {
					key = args[i+1]
					i++
				}
			case "-p", "--path":
				if i+1 < len(args) {
					destDir = args[i+1]
					i++
				}
			}
		}
		key = crypto.ResolveKey(key)
		idx, chunks, err := prov.DownloadRelease(relKey)
		if err != nil {
			return err
		}
		manifest, err := cm.UnpackFromChunks(idx, chunks, key, destDir)
		if err != nil {
			return err
		}
		res, err := eng.Restore(destDir, manifest, nil, true)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m☁ Release %s descargada y restaurada en %s:\033[0m (%d archivos en %d ms)\n",
			relKey, destDir, res.FilesCreated+res.FilesUpdated+res.FilesSkippedUnchanged, res.DurationMs)
		return nil

	case "status", "list", "ls":
		zkBadge := "\033[32m🔒 [ON]\033[0m"
		if !v.Config.Features.ZeroKnowledgeEncryption {
			zkBadge = "\033[33m🔓 [OFF]\033[0m"
		}
		fmt.Printf("\033[36m☁ Sincronización Cloud Multi-Backend:\033[0m\n")
		fmt.Printf("  • Proveedor Activo:       %s\n", emptyFallback(v.Config.Cloud.ActiveProvider, "none"))
		fmt.Printf("  • Repo GitHub Releases:   %s\n", emptyFallback(v.Config.Cloud.GitHubRepo, "(no configurado)"))
		fmt.Printf("  • Bucket S3 / R2:         %s (%s)\n", emptyFallback(v.Config.Cloud.S3Bucket, "-"), emptyFallback(v.Config.Cloud.S3Endpoint, "-"))
		fmt.Printf("  • Tamaño Máx por Chunk:   %d MB (Límite GitHub: 2048 MB)\n", v.Config.Cloud.ChunkLimitMB)
		fmt.Printf("  • Cifrado Zero-Knowledge: %s\n", zkBadge)
		return nil

	default:
		return fmt.Errorf("subcomando de cloud desconocido %q (válidos: status, setup, push, pull, list)", sub)
	}
}

func resolveCloudProvider(v *vault.Vault) (cloud.Provider, error) {
	switch v.Config.Cloud.ActiveProvider {
	case "github":
		return cloud.NewGitHubReleasesProvider(v.Config.Cloud.GitHubRepo, ""), nil
	case "s3":
		return cloud.NewS3CompatibleProvider(v.Config.Cloud.S3Endpoint, v.Config.Cloud.S3Bucket, v.Config.Cloud.S3Region), nil
	case "folder":
		return cloud.NewFolderProvider(v.Config.Cloud.S3Endpoint), nil
	default:
		return nil, fmt.Errorf("no hay ningún proveedor cloud configurado (ejecuta 'pfrost cloud setup github <usuario/repo>' o 's3')")
	}
}

func handleTeleportCmd(v *vault.Vault, eng *engine.Engine, cm *cloud.Manager, cwd string, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}

	switch sub {
	case "on", "enable":
		if err := v.Config.SetFeature("teleport", true); err != nil {
			return err
		}
		v.AppendAuditLog("TELEPORT_TOGGLED", "enabled=true")
		fmt.Println("\033[32m✔ Módulo Teleport ACTIVADO [ON]\033[0m")
		return nil

	case "off", "disable":
		if err := v.Config.SetFeature("teleport", false); err != nil {
			return err
		}
		v.AppendAuditLog("TELEPORT_TOGGLED", "enabled=false")
		fmt.Println("\033[33mℹ Módulo Teleport DESACTIVADO [OFF]\033[0m")
		return nil

	case "send", "create", "share":
		proj, err := v.FindProject(cwd)
		if err != nil {
			return err
		}
		targetRef := proj.ActiveSnapshotID
		key := ""
		for i := 1; i < len(args); i++ {
			if (args[i] == "--key" || args[i] == "-k") && i+1 < len(args) {
				key = args[i+1]
				i++
			} else if !strings.HasPrefix(args[i], "-") {
				targetRef = args[i]
			}
		}
		key = crypto.ResolveKey(key)
		manifest, err := v.LoadManifest(proj.ID, targetRef, false)
		if err != nil {
			return err
		}
		cap, err := cm.CreateTeleport(manifest, key, 24*time.Hour)
		if err != nil {
			return err
		}
		encBadge := "🔓 Sin cifrar"
		if cap.Encrypted {
			encBadge = "🔒 Cifrado Zero-Knowledge (AES-256-GCM)"
		}
		fmt.Printf("\033[32m🚀 Cápsula Teleport creada con éxito:\033[0m \033[1m%s\033[0m\n", cap.Code)
		fmt.Printf("  • Proyecto y Versión: %s (%s)\n", cap.ProjectName, cap.Tag)
		fmt.Printf("  • Seguridad:          %s\n", encBadge)
		fmt.Printf("  • Caducidad (TTL):    %s\n", cap.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
		fmt.Printf("  • Archivo Cápsula:    %s\n", cap.CapsulePath)
		fmt.Printf("  • Comando para recibir:\n      \033[36mpfrost teleport receive %s ./destino\033[0m\n", cap.Code)
		return nil

	case "receive", "pull":
		if len(args) < 2 {
			return fmt.Errorf("uso: pfrost teleport receive <codigo|archivo.pfteleport> [ruta-destino] [--key <clave>]")
		}
		codeOrFile := args[1]
		destDir := cwd
		key := ""
		for i := 2; i < len(args); i++ {
			if (args[i] == "--key" || args[i] == "-k") && i+1 < len(args) {
				key = args[i+1]
				i++
			} else if !strings.HasPrefix(args[i], "-") {
				destDir = args[i]
			}
		}
		key = crypto.ResolveKey(key)
		manifest, res, err := cm.ReceiveTeleport(eng, codeOrFile, destDir, key)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m🚀 Teleport recibido y reconstruido con éxito:\033[0m \033[1m%s (%s)\033[0m\n", manifest.ProjectName, manifest.Tag)
		fmt.Printf("  • Destino:  %s\n", destDir)
		fmt.Printf("  • Archivos: %d restaurados en %d ms\n", res.FilesCreated+res.FilesUpdated+res.FilesSkippedUnchanged, res.DurationMs)
		return nil

	case "revoke", "rm", "clear":
		target := "all"
		if len(args) > 1 {
			target = args[1]
		}
		n, err := cm.RevokeTeleport(target)
		if err != nil {
			return err
		}
		fmt.Printf("\033[32m✔ Revocadas y eliminadas %d cápsula(s) Teleport.\033[0m\n", n)
		return nil

	case "status", "list", "ls":
		badge := "\033[32m[ON ]\033[0m"
		if !cm.IsTeleportEnabled() {
			badge = "\033[31m[OFF]\033[0m"
		}
		fmt.Printf("\033[36m🚀 Módulo Permafrost Teleport:\033[0m %s\n", badge)
		list, _ := cm.ListTeleports()
		if len(list) == 0 {
			fmt.Println("  (No hay cápsulas Teleport activas. Usa 'pfrost teleport send' para compartir un estado)")
			return nil
		}
		for _, c := range list {
			enc := "🔓"
			if c.Encrypted {
				enc = "🔒"
			}
			fmt.Printf("  • \033[1m%-14s\033[0m %s | Proyecto: %-14s (%s) | Expira: %s\n",
				c.Code, enc, c.ProjectName, c.Tag, c.ExpiresAt.Local().Format("2006-01-02 15:04"))
		}
		return nil

	default:
		return fmt.Errorf("subcomando de teleport desconocido %q (válidos: status, list, send, receive, revoke, on, off)", sub)
	}
}

func printHookResults(results []hooks.CommandExecution, notice string, err error) error {
	if notice != "" {
		fmt.Println("\033[36m  " + notice + "\033[0m")
	}
	for _, r := range results {
		status := "\033[32m✔\033[0m"
		if !r.Success {
			status = "\033[31m✖\033[0m"
		}
		fmt.Printf("  %s \033[36m[Hook %s]\033[0m `%s` (%d ms)\n", status, r.Stage, r.Command, r.DurationMs)
		if r.Output != "" {
			fmt.Printf("      \033[90m↳ %s\033[0m\n", r.Output)
		}
	}
	return err
}

func printBanner(v *vault.Vault) {
	_ = time.Now()
	fmt.Printf("\033[36m\033[1m❄ PERMAFROST (pfrost)\033[0m v%s — Gestor Universal de Versiones & Snapshots por Bloques\n", version)
	fmt.Printf("  Vault Central: %s\n\n", v.RootDir)
}

func printHelp() {
	fmt.Println("Uso:")
	fmt.Println("  pfrost                               Abre la interfaz interactiva visual (TUI)")
	fmt.Println("  pfrost dashboard [--diff|--features] Renderiza el panel visual directamente en consola")
	fmt.Println("  pfrost init [ruta] [nombre]          Registra un proyecto o carpeta en el Vault")
	fmt.Println("  pfrost freeze [tag] [-m mensaje]     Congela el estado actual usando FastCDC + compresión")
	fmt.Println("  pfrost diff [v1] [v2] [-a]           Muestra el Time-Travel Diff visual con colores")
	fmt.Println("  pfrost pick <tag|id> <archivo...>    Restaura selectivamente archivos sueltos (Cherry-Pick)")
	fmt.Println("  pfrost switch <tag|id>               Cambia instantáneamente a la versión indicada (con Safe Switch)")
	fmt.Println("  pfrost safeswitch [status|on|off|..] Gestiona el módulo Safe Switch y los auto-stashes")
	fmt.Println("  pfrost hooks [status|init|trust|..]  Gestiona el módulo de Hooks (.permafrost.yaml)")
	fmt.Println("  pfrost bin [status|add|use|path|..]  Gestiona herramientas sueltas en ~/.permafrost/bin")
	fmt.Println("  pfrost crypto [status|keygen|on|off] Gestiona el cifrado Zero-Knowledge (AES-256-GCM)")
	fmt.Println("  pfrost cloud [status|setup|push|..]  Sincroniza con GitHub Releases (chunked <2GB) o S3/R2")
	fmt.Println("  pfrost teleport [send|receive|list]  Comparte entornos exactos mediante cápsulas Teleport")
	fmt.Println("  pfrost list                          Lista todas las versiones congeladas del proyecto actual")
	fmt.Println("  pfrost status                        Muestra estadísticas globales de deduplicación y ahorro")
	fmt.Println("  pfrost feature [modulo] [on|off]     Muestra o activa/desactiva módulos con total transparencia")
	fmt.Println("  pfrost gc                            Limpia bloques huérfanos no referenciados en el Vault")
	fmt.Println("  pfrost audit                         Muestra el registro transparente de operaciones")
}

func formatBytes(b int64) string {
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

func emptyFallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
