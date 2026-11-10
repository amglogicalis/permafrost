package diff

import (
	"fmt"
	"sort"
	"strings"

	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
)

// Status represents the change state of a file between two snapshots or working tree.
type Status string

const (
	StatusAdded     Status = "added"
	StatusModified  Status = "modified"
	StatusRemoved   Status = "removed"
	StatusUnchanged Status = "unchanged"
)

// FileDiff holds the comparison result for a single relative file path.
type FileDiff struct {
	RelPath   string `json:"rel_path"`
	Status    Status `json:"status"`
	OldSize   int64  `json:"old_size"`
	NewSize   int64  `json:"new_size"`
	DeltaSize int64  `json:"delta_size"`
	OldHash   string `json:"old_hash,omitempty"`
	NewHash   string `json:"new_hash,omitempty"`
}

// Report contains the complete Time-Travel Diff summary between Source and Target states.
type Report struct {
	SourceLabel    string     `json:"source_label"`
	TargetLabel    string     `json:"target_label"`
	AddedCount     int        `json:"added_count"`
	ModifiedCount  int        `json:"modified_count"`
	RemovedCount   int        `json:"removed_count"`
	UnchangedCount int        `json:"unchanged_count"`
	DeltaBytes     int64      `json:"delta_bytes"`
	Items          []FileDiff `json:"items"`
}

// HasChanges returns true if any file was added, modified, or removed.
func (r *Report) HasChanges() bool {
	return r.AddedCount > 0 || r.ModifiedCount > 0 || r.RemovedCount > 0
}

// CompareEntries computes the exact Time-Travel Diff from sourceEntries (old/current) to targetEntries (new/target).
func CompareEntries(sourceLabel string, sourceEntries []vault.FileEntry, targetLabel string, targetEntries []vault.FileEntry) *Report {
	srcMap := make(map[string]vault.FileEntry, len(sourceEntries))
	for _, f := range sourceEntries {
		srcMap[f.RelPath] = f
	}

	tgtMap := make(map[string]vault.FileEntry, len(targetEntries))
	for _, f := range targetEntries {
		tgtMap[f.RelPath] = f
	}

	allPathsMap := make(map[string]struct{}, len(srcMap)+len(tgtMap))
	for p := range srcMap {
		allPathsMap[p] = struct{}{}
	}
	for p := range tgtMap {
		allPathsMap[p] = struct{}{}
	}

	allPaths := make([]string, 0, len(allPathsMap))
	for p := range allPathsMap {
		allPaths = append(allPaths, p)
	}
	sort.Strings(allPaths)

	rep := &Report{
		SourceLabel: sourceLabel,
		TargetLabel: targetLabel,
		Items:       make([]FileDiff, 0, len(allPaths)),
	}

	for _, p := range allPaths {
		src, inSrc := srcMap[p]
		tgt, inTgt := tgtMap[p]

		switch {
		case !inSrc && inTgt:
			rep.AddedCount++
			rep.DeltaBytes += tgt.Size
			rep.Items = append(rep.Items, FileDiff{
				RelPath:   p,
				Status:    StatusAdded,
				OldSize:   0,
				NewSize:   tgt.Size,
				DeltaSize: tgt.Size,
				NewHash:   tgt.FileHash,
			})
		case inSrc && !inTgt:
			rep.RemovedCount++
			rep.DeltaBytes -= src.Size
			rep.Items = append(rep.Items, FileDiff{
				RelPath:   p,
				Status:    StatusRemoved,
				OldSize:   src.Size,
				NewSize:   0,
				DeltaSize: -src.Size,
				OldHash:   src.FileHash,
			})
		case inSrc && inTgt:
			if src.FileHash != tgt.FileHash || src.Size != tgt.Size {
				rep.ModifiedCount++
				delta := tgt.Size - src.Size
				rep.DeltaBytes += delta
				rep.Items = append(rep.Items, FileDiff{
					RelPath:   p,
					Status:    StatusModified,
					OldSize:   src.Size,
					NewSize:   tgt.Size,
					DeltaSize: delta,
					OldHash:   src.FileHash,
					NewHash:   tgt.FileHash,
				})
			} else {
				rep.UnchangedCount++
				rep.Items = append(rep.Items, FileDiff{
					RelPath:   p,
					Status:    StatusUnchanged,
					OldSize:   src.Size,
					NewSize:   tgt.Size,
					DeltaSize: 0,
					OldHash:   src.FileHash,
					NewHash:   tgt.FileHash,
				})
			}
		}
	}

	return rep
}

// CompareWorkingTreeToSnapshot compares a snapshot against the live working directory on disk.
// If previewSwitch is true, Source is Working Tree and Target is Snapshot (shows what switching to Snapshot will do).
// If previewSwitch is false, Source is Snapshot and Target is Working Tree (shows what you modified since Snapshot).
func CompareWorkingTreeToSnapshot(eng *engine.Engine, projectPath string, snap *vault.Manifest, previewSwitch bool) (*Report, error) {
	liveEntries, err := eng.ScanWorkingTree(projectPath)
	if err != nil {
		return nil, err
	}
	snapLabel := fmt.Sprintf("%s (%s)", snap.Tag, snap.ID)
	liveLabel := "Working Tree (Carpeta Actual)"

	if previewSwitch {
		return CompareEntries(liveLabel, liveEntries, snapLabel, snap.Files), nil
	}
	return CompareEntries(snapLabel, snap.Files, liveLabel, liveEntries), nil
}

// FormatColored outputs a rich ANSI-colored terminal representation of the Time-Travel Diff report.
func (r *Report) FormatColored(showUnchanged bool) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("\033[36m\033[1m❄ TIME-TRAVEL DIFF:\033[0m \033[1m%s\033[0m  ➔  \033[1m%s\033[0m\n", r.SourceLabel, r.TargetLabel))
	b.WriteString(fmt.Sprintf("  \033[32m🟢 +%d creados\033[0m  |  \033[33m🟡 ~%d modificados\033[0m  |  \033[31m🔴 -%d eliminados\033[0m  |  ⚪ %d intactos  |  Δ Tamaño: %s\n",
		r.AddedCount, r.ModifiedCount, r.RemovedCount, r.UnchangedCount, formatSignedBytes(r.DeltaBytes)))
	b.WriteString("  " + strings.Repeat("─", 78) + "\n")

	if !r.HasChanges() && !showUnchanged {
		b.WriteString("  \033[32m✔ No hay diferencias. Ambos estados son 100% idénticos byte a byte.\033[0m\n")
		return b.String()
	}

	for _, item := range r.Items {
		switch item.Status {
		case StatusAdded:
			b.WriteString(fmt.Sprintf("  \033[32m🟢 + %-48s\033[0m %12s (nuevo)\n",
				item.RelPath, formatUnsignedBytes(item.NewSize)))
		case StatusModified:
			b.WriteString(fmt.Sprintf("  \033[33m🟡 ~ %-48s\033[0m %12s (%s)\n",
				item.RelPath, formatUnsignedBytes(item.NewSize), formatSignedBytes(item.DeltaSize)))
		case StatusRemoved:
			b.WriteString(fmt.Sprintf("  \033[31m🔴 - %-48s\033[0m %12s (se eliminará)\n",
				item.RelPath, formatSignedBytes(-item.OldSize)))
		case StatusUnchanged:
			if showUnchanged {
				b.WriteString(fmt.Sprintf("  \033[90m⚪ = %-48s %12s (sin cambios)\033[0m\n",
					item.RelPath, formatUnsignedBytes(item.NewSize)))
			}
		}
	}
	return b.String()
}

func formatUnsignedBytes(b int64) string {
	if b < 0 {
		b = -b
	}
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

func formatSignedBytes(b int64) string {
	if b == 0 {
		return "0 B"
	}
	if b > 0 {
		return "+" + formatUnsignedBytes(b)
	}
	return "-" + formatUnsignedBytes(-b)
}
