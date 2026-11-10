#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");
const os = require("os");
const crypto = require("crypto");
const readline = require("readline");
const { spawnSync } = require("child_process");

// ============================================================================
// Permafrost (pfrost) — Native Windows Console Interactive TUI Controller
// Uses native Win32 Console Raw Mode via signed node.exe for 0ms keystroke
// latency while delegating heavy FastCDC block operations to the Go engine.
// ============================================================================

const ESC = "\x1b";
const CSI = "\x1b[";

const C = {
  reset: "\x1b[0m",
  bold: "\x1b[1m",
  dim: "\x1b[90m",
  cyan: "\x1b[36m",
  brightCyan: "\x1b[96m",
  teal: "\x1b[38;2;0;245;212m",
  green: "\x1b[32m",
  brightGreen: "\x1b[92m",
  yellow: "\x1b[33m",
  red: "\x1b[31m",
  white: "\x1b[97m",
  bgHeader: "\x1b[48;2;11;37;69m\x1b[38;2;0;229;255m\x1b[1m",
  bgTabActive: "\x1b[48;2;0;229;255m\x1b[38;2;11;37;69m\x1b[1m",
  bgTabInactive: "\x1b[48;2;19;49;92m\x1b[38;2;141;169;196m",
  bgSelected: "\x1b[48;2;19;49;92m\x1b[38;2;0;245;212m\x1b[1m"
};

function resolveVaultWinPath() {
  return path.join(os.homedir(), ".permafrost");
}

function toWslPath(winPath) {
  const abs = path.resolve(winPath).replace(/\\/g, "/");
  const m = abs.match(/^([A-Za-z]):\/(.*)$/);
  if (m) {
    return `/mnt/${m[1].toLowerCase()}/${m[2]}`;
  }
  return abs;
}

function fromWslPath(p) {
  if (!p) return p;
  const m = p.match(/^\/mnt\/([a-zA-Z])\/(.*)$/);
  if (m) {
    return `${m[1].toUpperCase()}:\\${m[2].replace(/\//g, "\\")}`;
  }
  return p;
}

function formatBytes(b) {
  b = Number(b) || 0;
  const abs = Math.abs(b);
  if (abs < 1024) return `${b} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let u = -1;
  let val = abs;
  while (val >= 1024 && u < units.length - 1) {
    val /= 1024;
    u++;
  }
  const sign = b < 0 ? "-" : "";
  return `${sign}${val.toFixed(2)} ${units[u]}`;
}

function readJSONSafe(filePath, fallback) {
  try {
    if (!fs.existsSync(filePath)) return fallback;
    return JSON.parse(fs.readFileSync(filePath, "utf8"));
  } catch (_) {
    return fallback;
  }
}

function writeJSONAtomic(filePath, obj) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  const tmp = `${filePath}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(obj, null, 2), "utf8");
  fs.renameSync(tmp, filePath);
}

function appendAudit(vaultDir, action, details) {
  try {
    const logPath = path.join(vaultDir, "audit.log");
    const line = `[${new Date().toISOString()}] ${action.padEnd(22)} | ${details}\n`;
    fs.appendFileSync(logPath, line, "utf8");
  } catch (_) {}
}

function runGoEngine(args, cwdWin) {
  const vaultWsl = toWslPath(resolveVaultWinPath());
  const cwdWsl = toWslPath(cwdWin || process.cwd());
  const quotedArgs = args.map((a) => `'${String(a).replace(/'/g, "'\\''")}'`).join(" ");
  const cmd = `export PFROST_VAULT_DIR='${vaultWsl}'; cd '${cwdWsl}' && exec "$HOME/.local/bin/pfrost" ${quotedArgs}`;
  const res = spawnSync("wsl.exe", ["-e", "bash", "-c", cmd], {
    encoding: "utf8"
  });
  const out = ((res.stdout || "") + "\n" + (res.stderr || "")).trim();
  const cleanOut = out.replace(/\x1b\[[0-9;]*m/g, "");
  return {
    ok: res.status === 0,
    output: cleanOut
  };
}

// ============================================================================
// State Management
// ============================================================================

const state = {
  vaultDir: resolveVaultWinPath(),
  cwd: process.cwd(),
  activeTab: 0, // 0: Snapshots, 1: Diff & Pick, 2: SafeSwitch, 3: Binaries & Hooks, 4: Cloud & Teleport, 5: Features & Audit
  config: null,
  projects: [],
  projectCursor: 0,
  snapshots: [],
  snapCursor: 0,
  stashes: [],
  stashCursor: 0,
  diffReport: null,
  diffCursor: 0,
  selectedDiffFiles: {},
  features: [],
  featCursor: 0,
  teleports: [],
  teleportCursor: 0,
  binaries: [],
  binCursor: 0,
  auditLogs: [],
  vaultStats: { projectCount: 0, snapshotCount: 0, stashCount: 0, blobCount: 0, physicalBytes: 0, logicalBytes: 0, savingsPct: 0 },
  statusMsg: "Usa las flechas [↑/↓/←/→] o [Tab / 1-6] para moverte por la interfaz.",
  modal: null // { type: 'freeze' | 'promote' | 'teleport', step: 1, field1: '', field2: '' }
};

const FEATURE_META = [
  { key: "safe_switch", title: "Safe Switch (Auto-Stash)", desc: "Crea automáticamente un snapshot de respaldo si detecta cambios sin guardar antes de cambiar de versión." },
  { key: "hooks", title: "Hooks Pre-Freeze / Post-Switch", desc: "Ejecuta scripts de .permafrost.yaml antes de congelar o después de cambiar de versión con auditoría previa." },
  { key: "time_travel_diff", title: "Time-Travel Visual Diff", desc: "Muestra el resumen visual de archivos creados, modificados y eliminados antes de cambiar de versión." },
  { key: "zero_knowledge_encryption", title: "Zero-Knowledge Cloud Encryption", desc: "Cifra todos los bloques y manifiestos localmente (AES-256-GCM) antes de subirlos a la nube." },
  { key: "teleport", title: "Teleport (Instant Environment Share)", desc: "Permite empaquetar y compartir estados exactos de proyectos mediante cápsulas efímeras." },
  { key: "global_binaries", title: "Permafrost Global Binaries", desc: "Gestiona y cambia versiones de ejecutables sueltos (terraform, node, etc.) en ~/.permafrost/bin." }
];

function refreshAll() {
  const vDir = state.vaultDir;
  fs.mkdirSync(path.join(vDir, "store", "blobs"), { recursive: true });
  fs.mkdirSync(path.join(vDir, "store", "manifests"), { recursive: true });
  fs.mkdirSync(path.join(vDir, "stash"), { recursive: true });
  fs.mkdirSync(path.join(vDir, "bin"), { recursive: true });
  fs.mkdirSync(path.join(vDir, "teleports"), { recursive: true });

  const defaultCfg = {
    version: "1.0.0",
    vault_dir: toWslPath(vDir),
    features: {
      safe_switch: true,
      hooks: true,
      time_travel_diff: true,
      zero_knowledge_encryption: true,
      teleport: true,
      global_binaries: true
    },
    chunking: { min_chunk_size: 32768, avg_chunk_size: 131072, max_chunk_size: 524288, compression_level: "default", max_auto_stashes: 15 },
    cloud: { active_provider: "none", github_repo: "", chunk_limit_mb: 100, s3_endpoint: "", s3_bucket: "", s3_region: "" },
    default_ignores: [".permafrost", ".permafrost-test"]
  };
  state.config = readJSONSafe(path.join(vDir, "config.json"), defaultCfg);
  if (!state.config.features) state.config.features = defaultCfg.features;
  if (!state.config.cloud) state.config.cloud = defaultCfg.cloud;

  state.features = FEATURE_META.map((m) => ({
    ...m,
    enabled: Boolean(state.config.features[m.key])
  }));

  // Load registry
  const reg = readJSONSafe(path.join(vDir, "registry.json"), { projects: {} });
  const projMap = reg.projects || {};
  state.projects = Object.values(projMap).sort((a, b) => new Date(b.updated_at || 0) - new Date(a.updated_at || 0));

  if (state.projectCursor >= state.projects.length) {
    state.projectCursor = Math.max(0, state.projects.length - 1);
  }

  // Load snapshots & stashes for active project
  if (state.projects.length > 0) {
    const activeProj = state.projects[state.projectCursor];
    state.snapshots = loadManifestList(path.join(vDir, "store", "manifests", activeProj.id));
    state.stashes = loadManifestList(path.join(vDir, "stash", activeProj.id));
  } else {
    state.snapshots = [];
    state.stashes = [];
  }

  if (state.snapCursor >= state.snapshots.length) {
    state.snapCursor = Math.max(0, state.snapshots.length - 1);
  }
  if (state.stashCursor >= state.stashes.length) {
    state.stashCursor = Math.max(0, state.stashes.length - 1);
  }

  computeVaultStats();
  computeLiveDiff();
  loadBinariesAndTeleports();
  loadAuditLogs();
}

function loadManifestList(folder) {
  if (!fs.existsSync(folder)) return [];
  const files = fs.readdirSync(folder).filter((f) => f.endsWith(".json"));
  const list = [];
  for (const f of files) {
    const m = readJSONSafe(path.join(folder, f), null);
    if (m && m.id) list.push(m);
  }
  list.sort((a, b) => new Date(b.created_at || 0) - new Date(a.created_at || 0));
  return list;
}

function computeVaultStats() {
  const vDir = state.vaultDir;
  let snapCount = 0;
  let stashCount = 0;
  let logicalBytes = 0;

  for (const p of state.projects) {
    const snaps = loadManifestList(path.join(vDir, "store", "manifests", p.id));
    snapCount += snaps.length;
    for (const s of snaps) logicalBytes += (s.stats && s.stats.logical_bytes) || 0;

    const stashes = loadManifestList(path.join(vDir, "stash", p.id));
    stashCount += stashes.length;
    for (const st of stashes) logicalBytes += (st.stats && st.stats.logical_bytes) || 0;
  }

  let blobCount = 0;
  let physicalBytes = 0;
  const blobsDir = path.join(vDir, "store", "blobs");
  if (fs.existsSync(blobsDir)) {
    for (const sub of fs.readdirSync(blobsDir)) {
      const subPath = path.join(blobsDir, sub);
      if (fs.statSync(subPath).isDirectory()) {
        for (const bf of fs.readdirSync(subPath)) {
          if (bf.endsWith(".zst")) {
            blobCount++;
            physicalBytes += fs.statSync(path.join(subPath, bf)).size;
          }
        }
      }
    }
  }

  let savingsPct = 0;
  if (logicalBytes > 0 && logicalBytes > physicalBytes) {
    savingsPct = ((logicalBytes - physicalBytes) / logicalBytes) * 100;
  }

  state.vaultStats = {
    projectCount: state.projects.length,
    snapshotCount: snapCount,
    stashCount,
    blobCount,
    physicalBytes,
    logicalBytes,
    savingsPct
  };
}

function computeLiveDiff() {
  if (state.projects.length === 0 || state.snapshots.length === 0) {
    state.diffReport = null;
    return;
  }
  const proj = state.projects[state.projectCursor];
  const snap = state.snapshots[state.snapCursor];
  const winRoot = fromWslPath(proj.path);

  const liveMap = {};
  if (fs.existsSync(winRoot)) {
    walkLiveFiles(winRoot, winRoot, liveMap);
  }

  const snapMap = {};
  for (const f of snap.files || []) {
    snapMap[f.rel_path] = f;
  }

  const allPaths = Array.from(new Set([...Object.keys(liveMap), ...Object.keys(snapMap)])).sort();
  const items = [];
  let added = 0;
  let modified = 0;
  let removed = 0;
  let unchanged = 0;

  for (const rel of allPaths) {
    const live = liveMap[rel];
    const tgt = snapMap[rel];
    if (!live && tgt) {
      added++;
      items.push({ rel_path: rel, status: "added", old_size: 0, new_size: tgt.size, delta: tgt.size });
    } else if (live && !tgt) {
      removed++;
      items.push({ rel_path: rel, status: "removed", old_size: live.size, new_size: 0, delta: -live.size });
    } else if (live && tgt) {
      if (live.size !== tgt.size || live.hash !== tgt.file_hash) {
        modified++;
        items.push({ rel_path: rel, status: "modified", old_size: live.size, new_size: tgt.size, delta: tgt.size - live.size });
      } else {
        unchanged++;
        items.push({ rel_path: rel, status: "unchanged", old_size: live.size, new_size: tgt.size, delta: 0 });
      }
    }
  }

  // Sort changed items first so the user sees modifications immediately
  const order = { modified: 0, added: 1, removed: 2, unchanged: 3 };
  items.sort((a, b) => (order[a.status] - order[b.status]) || a.rel_path.localeCompare(b.rel_path));

  state.diffReport = {
    sourceLabel: "Carpeta Actual",
    targetLabel: `${snap.tag} (${snap.id})`,
    added,
    modified,
    removed,
    unchanged,
    items
  };

  if (state.diffCursor >= items.length) {
    state.diffCursor = Math.max(0, items.length - 1);
  }
}

function walkLiveFiles(rootDir, currentDir, outMap) {
  let entries;
  try {
    entries = fs.readdirSync(currentDir, { withFileTypes: true });
  } catch (_) {
    return;
  }
  for (const e of entries) {
    if (e.name === ".permafrost" || e.name === ".permafrost-test" || (e.name.includes(".pfrost.") && e.name.endsWith(".tmp"))) {
      continue;
    }
    const full = path.join(currentDir, e.name);
    const rel = path.relative(rootDir, full).replace(/\\/g, "/");
    if (e.isDirectory()) {
      walkLiveFiles(rootDir, full, outMap);
    } else if (e.isFile()) {
      try {
        const st = fs.statSync(full);
        const buf = fs.readFileSync(full);
        const h = crypto.createHash("sha256").update(buf).digest("hex");
        outMap[rel] = { size: st.size, hash: h };
      } catch (_) {}
    }
  }
}

function loadBinariesAndTeleports() {
  const binCat = readJSONSafe(path.join(state.vaultDir, "binaries.json"), { tools: {} });
  state.binaries = Object.values(binCat.tools || {});

  const tpDir = path.join(state.vaultDir, "teleports");
  state.teleports = [];
  if (fs.existsSync(tpDir)) {
    for (const f of fs.readdirSync(tpDir)) {
      if (f.endsWith(".pfteleport")) {
        const cap = readJSONSafe(path.join(tpDir, f), null);
        if (cap && cap.code) state.teleports.push(cap);
      }
    }
  }
  state.teleports.sort((a, b) => new Date(b.created_at || 0) - new Date(a.created_at || 0));
}

function loadAuditLogs() {
  const logPath = path.join(state.vaultDir, "audit.log");
  if (!fs.existsSync(logPath)) {
    state.auditLogs = [];
    return;
  }
  const lines = fs.readFileSync(logPath, "utf8").split(/\r?\n/).filter((l) => l.trim() !== "");
  state.auditLogs = lines.slice(-14).reverse();
}

// ============================================================================
// Rendering Engine
// ============================================================================

function render() {
  const out = [];
  out.push(CSI + "H" + CSI + "2J"); // Clear screen & home cursor

  const st = state.vaultStats;
  out.push(
    `${C.bgHeader} ❄ PERMAFROST (pfrost)  •  Proyectos: ${st.projectCount}  •  Snapshots: ${st.snapshotCount} (+${st.stashCount} stashes)  •  Ahorro Deduplicación: ${st.savingsPct.toFixed(1)}% ${C.reset}\n`
  );

  // Render 6 interactive tabs
  const tabs = [
    "[1] ❄ Snapshots",
    "[2] 🔍 Diff & Pick",
    "[3] 🛡 Safe Switch",
    "[4] 🧰 Binaries & Hooks",
    "[5] ☁ Cloud & Teleport",
    "[6] ⚙ Módulos ON/OFF & Audit"
  ];
  const tabLine = tabs
    .map((t, idx) => (idx === state.activeTab ? `${C.bgTabActive} ${t} ${C.reset}` : `${C.bgTabInactive} ${t} ${C.reset}`))
    .join(" ");
  out.push(tabLine + "\n");

  // Modal Overlay if active
  if (state.modal) {
    out.push(renderModal());
    process.stdout.write(out.join("\n"));
    return;
  }

  switch (state.activeTab) {
    case 0:
      out.push(renderSnapshotsTab());
      break;
    case 1:
      out.push(renderDiffTab());
      break;
    case 2:
      out.push(renderSafeSwitchTab());
      break;
    case 3:
      out.push(renderBinariesHooksTab());
      break;
    case 4:
      out.push(renderCloudTeleportTab());
      break;
    case 5:
      out.push(renderFeaturesAuditTab());
      break;
  }

  out.push(`\n${C.teal}ℹ ${state.statusMsg}${C.reset}`);
  out.push(
    `${C.dim}Controles: [↑/↓] Mover cursor • [Tab / 1-6] Cambiar Pestaña • [f] Congelar • [Enter/s] Aplicar/Activar • [d] Diff • [q] Salir${C.reset}`
  );

  process.stdout.write(out.join("\n"));
}

function renderModal() {
  const m = state.modal;
  const lines = [];
  lines.push("╭──────────────────────────────────────────────────────────────────────────────────────────────╮");
  if (m.type === "freeze") {
    lines.push(`│  ${C.brightCyan}${C.bold}❄ CONGELAR NUEVA VERSIÓN (SNAPSHOT FASTCDC)${C.reset}                                               │`);
    lines.push("│                                                                                              │");
    const c1 = m.step === 1 ? "█" : "";
    const c2 = m.step === 2 ? "█" : "";
    lines.push(`│  1. Tag de la versión:  ${C.teal}${m.field1}${c1}${C.reset}`);
    lines.push(`│  2. Mensaje / Nota:     ${C.white}${m.field2}${c2}${C.reset}`);
    lines.push("│                                                                                              │");
    lines.push(`│  ${C.dim}Pulsa [Enter] para confirmar paso o [Esc] para cancelar.${C.reset}                                  │`);
  }
  lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────╯");
  return lines.join("\n");
}

function renderSnapshotsTab() {
  const lines = [];
  lines.push("╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮");

  if (state.projects.length === 0) {
    lines.push(`│  No hay proyectos registrados todavía en ${state.vaultDir}.`);
    lines.push(`│  Pulsa ${C.brightCyan}[i]${C.reset} para registrar la carpeta actual (${state.cwd}) o ${C.brightCyan}[f]${C.reset} para congelar tu primera versión.`);
    lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
    return lines.join("\n");
  }

  const proj = state.projects[state.projectCursor];
  const winPath = fromWslPath(proj.path);
  lines.push(
    `│  Proyecto Activo (${state.projectCursor + 1}/${state.projects.length}): ${C.teal}${C.bold}${proj.name}${C.reset}  ${C.dim}(${winPath}) — Usa [←/→] para cambiar de proyecto${C.reset}`
  );
  lines.push("│  " + "─".repeat(110));

  if (state.snapshots.length === 0) {
    lines.push(`│  (Sin versiones congeladas aún. Pulsa ${C.brightCyan}[f]${C.reset} para congelar el estado actual)`);
  } else {
    state.snapshots.forEach((s, idx) => {
      const isSel = idx === state.snapCursor;
      const cursor = isSel ? "❯ " : "  ";
      const activeMark = s.id === proj.active_snapshot_id ? `${C.brightGreen}[*]${C.reset}` : "   ";
      const filesCount = (s.stats && s.stats.total_files) || 0;
      const newBytes = formatBytes(s.stats && s.stats.new_bytes_stored);
      const savedPct = ((s.stats && s.stats.space_saved_percent) || 0).toFixed(1);
      const dt = new Date(s.created_at).toLocaleString();
      const rowText = `${cursor}${activeMark} ${s.tag.padEnd(14)} │ ${s.id.padEnd(28)} │ ${String(filesCount).padStart(4)} arch │ Nuevo: ${newBytes.padEnd(10)} (${savedPct}% ahorro) │ ${dt} │ ${s.message || ""}`;
      lines.push("│  " + (isSel ? `${C.bgSelected}${rowText}${C.reset}` : rowText));
    });
  }

  if (state.diffReport) {
    const dr = state.diffReport;
    const snapTag = state.snapshots[state.snapCursor] ? state.snapshots[state.snapCursor].tag : "-";
    lines.push("│  " + "─".repeat(110));
    lines.push(
      `│  Vista Previa Time-Travel Diff (Carpeta Actual ➔ ${C.bold}${snapTag}${C.reset}):  ${C.brightGreen}🟢 +${dr.added} creados${C.reset}  │  ${C.yellow}🟡 ~${dr.modified} modificados${C.reset}  │  ${C.red}🔴 -${dr.removed} eliminados${C.reset}  │  ⚪ ${dr.unchanged} intactos`
    );
  }

  lines.push("│");
  lines.push(
    `│  ${C.brightCyan}Acciones en esta pestaña:${C.reset} ${C.bold}[↑/↓]${C.reset} Elegir versión • ${C.bold}[Enter / s]${C.reset} Hot-Swap a versión • ${C.bold}[f]${C.reset} Congelar nueva • ${C.bold}[d]${C.reset} Abrir Diff/Cherry-Pick • ${C.bold}[x]${C.reset} Borrar • ${C.bold}[g]${C.reset} GC`
  );
  lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
  return lines.join("\n");
}

function renderDiffTab() {
  const lines = [];
  lines.push("╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮");
  if (!state.diffReport) {
    lines.push("│  Selecciona primero un proyecto con al menos un snapshot en la pestaña [1].");
    lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
    return lines.join("\n");
  }

  const dr = state.diffReport;
  lines.push(`│  ${C.brightCyan}${C.bold}🔍 TIME-TRAVEL VISUAL DIFF & CHERRY-PICK:${C.reset}  ${C.teal}${dr.sourceLabel}${C.reset}  ➔  ${C.teal}${dr.targetLabel}${C.reset}`);
  lines.push(
    `│  Resumen: ${C.brightGreen}🟢 +${dr.added} creados${C.reset}  │  ${C.yellow}🟡 ~${dr.modified} modificados${C.reset}  │  ${C.red}🔴 -${dr.removed} eliminados${C.reset}  │  ⚪ ${dr.unchanged} intactos`
  );
  lines.push(`│  ${C.dim}Usa [↑/↓] para moverte, [Espacio] para marcar archivos [✓], y [Enter / r] para restaurar SOLO los marcados.${C.reset}`);
  lines.push("│  " + "─".repeat(110));

  const visibleItems = dr.items.slice(0, 16);
  visibleItems.forEach((it, idx) => {
    const isSel = idx === state.diffCursor;
    const cursor = isSel ? "❯ " : "  ";
    const checked = state.selectedDiffFiles[it.rel_path] ? `${C.brightGreen}[✓]${C.reset}` : "[ ]";
    let badge = `${C.dim}⚪ = INTACTO   ${C.reset}`;
    if (it.status === "added") badge = `${C.brightGreen}🟢 + CREADO    ${C.reset}`;
    else if (it.status === "modified") badge = `${C.yellow}🟡 ~ MODIFICADO${C.reset}`;
    else if (it.status === "removed") badge = `${C.red}🔴 - ELIMINADO ${C.reset}`;

    const line = `${cursor}${checked} ${badge} │ ${it.rel_path.padEnd(50)} │ ${formatBytes(it.new_size).padStart(10)} (${formatBytes(it.delta)})`;
    lines.push("│  " + (isSel ? `${C.bgSelected}${line}${C.reset}` : line));
  });

  if (dr.items.length > 16) {
    lines.push(`│  ${C.dim}... (+${dr.items.length - 16} archivos adicionales intactos)${C.reset}`);
  }

  lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
  return lines.join("\n");
}

function renderSafeSwitchTab() {
  const lines = [];
  const enabled = state.config.features.safe_switch;
  const badge = enabled ? `${C.brightGreen}[ON ]${C.reset}` : `${C.red}[OFF]${C.reset}`;

  lines.push("╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮");
  lines.push(`│  ${C.brightCyan}${C.bold}🛡 MÓDULO SAFE SWITCH (AUTO-STASH TRANSPARENTE):${C.reset} ${badge}  ${C.dim}(Pulsa [t] para Activar/Desactivar)${C.reset}`);
  lines.push("│  Cuando está [ON], si cambias de versión teniendo archivos modificados sin guardar, Permafrost crea un Auto-Stash.");
  lines.push("│  " + "─".repeat(110));

  if (state.stashes.length === 0) {
    lines.push("│  (No hay Auto-Stashes pendientes en este proyecto. Tu trabajo está limpio.)");
  } else {
    state.stashes.forEach((st, idx) => {
      const isSel = idx === state.stashCursor;
      const cursor = isSel ? "❯ " : "  ";
      const line = `${cursor}${st.tag.padEnd(26)} │ ${st.id.padEnd(28)} │ ${String(st.stats.total_files).padStart(4)} arch │ ${st.message}`;
      lines.push("│  " + (isSel ? `${C.bgSelected}${line}${C.reset}` : line));
    });
  }

  lines.push("│");
  lines.push(
    `│  ${C.brightCyan}Acciones:${C.reset} ${C.bold}[t]${C.reset} Alternar Safe Switch ON/OFF • ${C.bold}[Enter / r]${C.reset} Restaurar Auto-Stash • ${C.bold}[p]${C.reset} Promocionar a Versión Permanente • ${C.bold}[x]${C.reset} Limpiar`
  );
  lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
  return lines.join("\n");
}

function renderBinariesHooksTab() {
  const lines = [];
  const binBadge = state.config.features.global_binaries ? `${C.brightGreen}[ON ]${C.reset}` : `${C.red}[OFF]${C.reset}`;
  const hookBadge = state.config.features.hooks ? `${C.brightGreen}[ON ]${C.reset}` : `${C.red}[OFF]${C.reset}`;

  lines.push("╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮");
  lines.push(`│  ${C.brightCyan}${C.bold}🧰 PERMAFROST GLOBAL BINARIES:${C.reset} ${binBadge}   │   ${C.brightCyan}${C.bold}⚡ LIFECYCLE HOOKS (.permafrost.yaml):${C.reset} ${hookBadge}`);
  lines.push(`│  Directorio de Binarios Globales: ${C.teal}${path.join(state.vaultDir, "bin")}${C.reset}`);
  lines.push("│  " + "─".repeat(110));

  if (state.binaries.length === 0) {
    lines.push("│  • Herramientas Globales: (Ninguna registrada aún. Usa 'pfrost bin add <tool> <exe> <version>' o pulsa [b] demo)");
  } else {
    state.binaries.forEach((t, idx) => {
      const versions = Object.keys(t.versions || {}).join(", ");
      lines.push(`│  • ${C.teal}${C.bold}${t.name.padEnd(16)}${C.reset} │ Versión Activa: ${C.brightGreen}${t.active_version || "-"}${C.reset} │ Versiones congeladas: [${versions}]`);
    });
  }

  lines.push("│  " + "─".repeat(110));
  const proj = state.projects[state.projectCursor];
  const projWinPath = proj ? fromWslPath(proj.path) : state.cwd;
  const yamlPath = path.join(projWinPath, ".permafrost.yaml");
  const hasYaml = fs.existsSync(yamlPath);

  lines.push(`│  ${C.brightCyan}${C.bold}Estado de Hooks en Proyecto Actual (${projWinPath}):${C.reset}`);
  if (!hasYaml) {
    lines.push(`│  • No hay archivo .permafrost.yaml en este proyecto. Pulsa ${C.brightCyan}[i]${C.reset} para generar y autorizar la plantilla ahora mismo.`);
  } else {
    lines.push(`│  • Archivo detectado: ${C.brightGreen}${yamlPath}${C.reset}`);
    lines.push(`│  • Pulsa ${C.brightCyan}[a]${C.reset} para autorizar su firma SHA-256 o ${C.brightCyan}[r]${C.reset} para probar la ejecución de pre_freeze.`);
  }

  lines.push("│");
  lines.push(`│  ${C.brightCyan}Acciones:${C.reset} ${C.bold}[i]${C.reset} Crear plantilla .permafrost.yaml • ${C.bold}[a]${C.reset} Autorizar Hooks • ${C.bold}[r]${C.reset} Ejecutar Hook Test`);
  lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
  return lines.join("\n");
}

function renderCloudTeleportTab() {
  const lines = [];
  const zkBadge = state.config.features.zero_knowledge_encryption ? `${C.brightGreen}🔒 [ON ] AES-256-GCM${C.reset}` : `${C.yellow}🔓 [OFF] Plain${C.reset}`;
  const tpBadge = state.config.features.teleport ? `${C.brightGreen}[ON ]${C.reset}` : `${C.red}[OFF]${C.reset}`;

  lines.push("╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮");
  lines.push(`│  ${C.brightCyan}${C.bold}☁ CLOUD MULTI-BACKEND & ZERO-KNOWLEDGE:${C.reset} ${zkBadge}   │   ${C.brightCyan}${C.bold}🚀 TELEPORT:${C.reset} ${tpBadge}`);
  lines.push(`│  • Proveedor Cloud Activo: ${C.teal}${state.config.cloud.active_provider || "none"}${C.reset}  (GitHub Repo: ${state.config.cloud.github_repo || "no configurado"} | Chunk Limit: ${state.config.cloud.chunk_limit_mb || 100} MB)`);
  lines.push(`│  • Huella Clave Zero-Knowledge: ${C.teal}${state.config.cloud.key_check_hex || "No generada aún (pulsa [k] para generar Recovery Key)"}${C.reset}`);
  lines.push("│  " + "─".repeat(110));
  lines.push(`│  ${C.brightCyan}${C.bold}Cápsulas Teleport Activas en ~/.permafrost/teleports (${state.teleports.length}):${C.reset}`);

  if (state.teleports.length === 0) {
    lines.push(`│  (No hay cápsulas Teleport activas. Pulsa ${C.brightCyan}[t]${C.reset} para crear una cápsula Teleport instantánea de la versión activa)`);
  } else {
    state.teleports.forEach((tp, idx) => {
      const isSel = idx === state.teleportCursor;
      const cursor = isSel ? "❯ " : "  ";
      const enc = tp.encrypted ? "🔒 Cifrado" : "🔓 Plain  ";
      const exp = new Date(tp.expires_at).toLocaleString();
      const row = `${cursor}${tp.code.padEnd(14)} │ ${enc} │ Proyecto: ${tp.project_name} (${tp.tag}) │ Expira: ${exp}`;
      lines.push("│  " + (isSel ? `${C.bgSelected}${row}${C.reset}` : row));
    });
  }

  lines.push("│");
  lines.push(
    `│  ${C.brightCyan}Acciones:${C.reset} ${C.bold}[k]${C.reset} Generar Recovery Key Zero-Knowledge • ${C.bold}[t]${C.reset} Crear Cápsula Teleport ahora • ${C.bold}[x]${C.reset} Revocar Teleport seleccionado`
  );
  lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
  return lines.join("\n");
}

function renderFeaturesAuditTab() {
  const lines = [];
  lines.push("╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮");
  lines.push(`│  ${C.brightCyan}${C.bold}⚙ PANEL DE CONTROL DE MÓDULOS (Pulsa [↑/↓] y [Espacio / Enter] para Activar [ON] o Desactivar [OFF]):${C.reset}`);
  lines.push("│  " + "─".repeat(110));

  state.features.forEach((f, idx) => {
    const isSel = idx === state.featCursor;
    const cursor = isSel ? "❯ " : "  ";
    const badge = f.enabled ? `${C.brightGreen}[ON ]${C.reset}` : `${C.red}[OFF]${C.reset}`;
    const title = isSel ? `${C.bgSelected} ${f.title.padEnd(35)} ${C.reset}` : f.title.padEnd(35);
    lines.push(`│  ${cursor}${badge} ${title} │ ${C.dim}${f.desc}${C.reset}`);
  });

  lines.push("│  " + "─".repeat(110));
  lines.push(`│  ${C.brightCyan}${C.bold}🛡 Últimos Eventos en Registro Transparente (~/.permafrost/audit.log):${C.reset}`);
  if (state.auditLogs.length === 0) {
    lines.push("│  (Sin eventos registrados)");
  } else {
    state.auditLogs.slice(0, 6).forEach((l) => {
      lines.push(`│  ${C.dim}${l.slice(0, 110)}${C.reset}`);
    });
  }
  lines.push("╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯");
  return lines.join("\n");
}

// ============================================================================
// Keypress Handler
// ============================================================================

function handleKey(str, key) {
  if (!key) return;

  if (key.ctrl && key.name === "c") {
    cleanupAndExit();
  }

  // Modal Input Handling
  if (state.modal) {
    if (key.name === "escape") {
      state.modal = null;
      state.statusMsg = "Operación cancelada.";
      render();
      return;
    }
    if (key.name === "return" || key.name === "enter") {
      if (state.modal.step === 1) {
        state.modal.step = 2;
        render();
        return;
      }
      const tag = state.modal.field1.trim() || `v${state.snapshots.length + 1}`;
      const msg = state.modal.field2.trim() || "Snapshot desde Permafrost TUI";
      state.modal = null;
      state.statusMsg = `Congelando versión ${tag}...`;
      render();
      const proj = state.projects[state.projectCursor];
      const targetWinPath = proj ? fromWslPath(proj.path) : state.cwd;
      const res = runGoEngine(["freeze", tag, "-m", msg], targetWinPath);
      state.statusMsg = res.ok ? `✔ Versión ${tag} congelada con éxito.` : `✖ Error: ${res.output}`;
      refreshAll();
      render();
      return;
    }
    if (key.name === "backspace") {
      if (state.modal.step === 1) state.modal.field1 = state.modal.field1.slice(0, -1);
      else state.modal.field2 = state.modal.field2.slice(0, -1);
      render();
      return;
    }
    if (str && str.length === 1 && !key.ctrl && !key.meta && str >= " ") {
      if (state.modal.step === 1) state.modal.field1 += str;
      else state.modal.field2 += str;
      render();
    }
    return;
  }

  // Global Shortcuts
  if (key.name === "q" || key.name === "escape") {
    cleanupAndExit();
  }
  if (key.name === "tab") {
    state.activeTab = key.shift ? (state.activeTab + 5) % 6 : (state.activeTab + 1) % 6;
    render();
    return;
  }
  if (str >= "1" && str <= "6") {
    state.activeTab = Number(str) - 1;
    render();
    return;
  }

  // Tab-specific navigation
  switch (state.activeTab) {
    case 0:
      handleSnapshotsKeys(str, key);
      break;
    case 1:
      handleDiffKeys(str, key);
      break;
    case 2:
      handleSafeSwitchKeys(str, key);
      break;
    case 3:
      handleBinariesHooksKeys(str, key);
      break;
    case 4:
      handleCloudTeleportKeys(str, key);
      break;
    case 5:
      handleFeaturesAuditKeys(str, key);
      break;
  }
  render();
}

function handleSnapshotsKeys(str, key) {
  if (key.name === "up" || str === "k") {
    if (state.snapCursor > 0) {
      state.snapCursor--;
      computeLiveDiff();
    }
  } else if (key.name === "down" || str === "j") {
    if (state.snapCursor + 1 < state.snapshots.length) {
      state.snapCursor++;
      computeLiveDiff();
    }
  } else if (key.name === "left" || str === "h") {
    if (state.projectCursor > 0) {
      state.projectCursor--;
      state.snapCursor = 0;
      refreshAll();
    }
  } else if (key.name === "right" || str === "l") {
    if (state.projectCursor + 1 < state.projects.length) {
      state.projectCursor++;
      state.snapCursor = 0;
      refreshAll();
    }
  } else if (str === "i") {
    const res = runGoEngine(["init", toWslPath(state.cwd)], state.cwd);
    state.statusMsg = res.ok ? `✔ Proyecto inicializado (${state.cwd})` : res.output;
    refreshAll();
  } else if (str === "f") {
    state.modal = {
      type: "freeze",
      step: 1,
      field1: `v1.${state.snapshots.length}`,
      field2: "Snapshot interactivo desde TUI"
    };
  } else if (key.name === "return" || key.name === "enter" || str === "s") {
    if (state.projects.length > 0 && state.snapshots.length > 0) {
      const proj = state.projects[state.projectCursor];
      const snap = state.snapshots[state.snapCursor];
      const winPath = fromWslPath(proj.path);
      state.statusMsg = `Cambiando a versión ${snap.tag}...`;
      render();
      const res = runGoEngine(["switch", snap.id], winPath);
      state.statusMsg = res.ok ? `⚡ Hot-Swap completado a ${snap.tag} (${snap.id})` : `✖ ${res.output}`;
      refreshAll();
    }
  } else if (str === "d") {
    state.activeTab = 1;
  } else if (str === "c") {
    state.activeTab = 5;
  } else if (str === "x" || key.name === "delete") {
    if (state.projects.length > 0 && state.snapshots.length > 0) {
      const proj = state.projects[state.projectCursor];
      const snap = state.snapshots[state.snapCursor];
      const p = path.join(state.vaultDir, "store", "manifests", proj.id, `${snap.id}.json`);
      if (fs.existsSync(p)) fs.unlinkSync(p);
      appendAudit(state.vaultDir, "MANIFEST_DELETED", `Deleted ${snap.id} (${snap.tag}) via TUI`);
      state.statusMsg = `🗑 Snapshot ${snap.tag} eliminado.`;
      refreshAll();
    }
  } else if (str === "g") {
    const res = runGoEngine(["gc"], state.cwd);
    state.statusMsg = res.ok ? "♻ Recolección de basura (GC) completada." : res.output;
    refreshAll();
  }
}

function handleDiffKeys(str, key) {
  if (!state.diffReport) return;
  const items = state.diffReport.items;
  if (key.name === "up" || str === "k") {
    if (state.diffCursor > 0) state.diffCursor--;
  } else if (key.name === "down" || str === "j") {
    if (state.diffCursor + 1 < items.length) state.diffCursor++;
  } else if (key.name === "space" || str === " ") {
    if (state.diffCursor < items.length) {
      const rel = items[state.diffCursor].rel_path;
      state.selectedDiffFiles[rel] = !state.selectedDiffFiles[rel];
      if (!state.selectedDiffFiles[rel]) delete state.selectedDiffFiles[rel];
    }
  } else if (key.name === "return" || key.name === "enter" || str === "r") {
    if (state.projects.length > 0 && state.snapshots.length > 0) {
      const proj = state.projects[state.projectCursor];
      const snap = state.snapshots[state.snapCursor];
      let files = Object.keys(state.selectedDiffFiles);
      if (files.length === 0 && state.diffCursor < items.length) {
        files = [items[state.diffCursor].rel_path];
      }
      if (files.length > 0) {
        const res = runGoEngine(["pick", snap.id, ...files], fromWslPath(proj.path));
        state.selectedDiffFiles = {};
        state.statusMsg = res.ok ? `🍒 Cherry-Pick completado: ${files.join(", ")} desde ${snap.tag}` : `✖ ${res.output}`;
        refreshAll();
      }
    }
  }
}

function handleSafeSwitchKeys(str, key) {
  if (key.name === "up" || str === "k") {
    if (state.stashCursor > 0) state.stashCursor--;
  } else if (key.name === "down" || str === "j") {
    if (state.stashCursor + 1 < state.stashes.length) state.stashCursor++;
  } else if (str === "t") {
    toggleFeatureByKey("safe_switch");
  } else if ((key.name === "return" || key.name === "enter" || str === "r") && state.stashes.length > 0) {
    const proj = state.projects[state.projectCursor];
    const st = state.stashes[state.stashCursor];
    const res = runGoEngine(["safeswitch", "restore", st.id], fromWslPath(proj.path));
    state.statusMsg = res.ok ? `✔ Auto-Stash ${st.tag} restaurado.` : res.output;
    refreshAll();
  } else if (str === "p" && state.stashes.length > 0) {
    const proj = state.projects[state.projectCursor];
    const st = state.stashes[state.stashCursor];
    const newTag = `promoted-${st.tag.slice(0, 14)}`;
    const res = runGoEngine(["safeswitch", "promote", st.id, newTag], fromWslPath(proj.path));
    state.statusMsg = res.ok ? `✔ Auto-Stash promocionado a versión permanente ${newTag}.` : res.output;
    refreshAll();
  } else if (str === "x" && state.projects.length > 0) {
    const proj = state.projects[state.projectCursor];
    const res = runGoEngine(["safeswitch", "clear"], fromWslPath(proj.path));
    state.statusMsg = res.ok ? "🗑 Auto-stashes limpiados." : res.output;
    refreshAll();
  }
}

function handleBinariesHooksKeys(str, key) {
  const proj = state.projects[state.projectCursor];
  const winPath = proj ? fromWslPath(proj.path) : state.cwd;
  if (str === "i") {
    const res = runGoEngine(["hooks", "init"], winPath);
    state.statusMsg = res.ok ? "✔ Plantilla .permafrost.yaml creada y autorizada." : res.output;
    refreshAll();
  } else if (str === "a") {
    const res = runGoEngine(["hooks", "trust"], winPath);
    state.statusMsg = res.ok ? "✔ Firma SHA-256 de .permafrost.yaml autorizada." : res.output;
    refreshAll();
  } else if (str === "r") {
    const res = runGoEngine(["hooks", "run", "pre_freeze"], winPath);
    state.statusMsg = res.ok ? "⚡ Hook pre_freeze ejecutado con éxito." : res.output;
    refreshAll();
  }
}

function handleCloudTeleportKeys(str, key) {
  if (key.name === "up" || str === "k") {
    if (state.teleportCursor > 0) state.teleportCursor--;
  } else if (key.name === "down" || str === "j") {
    if (state.teleportCursor + 1 < state.teleports.length) state.teleportCursor++;
  } else if (str === "k") {
    const res = runGoEngine(["crypto", "keygen"], state.cwd);
    state.statusMsg = res.ok ? "🔑 Nueva Recovery Key Zero-Knowledge generada (revisa 'pfrost crypto status')." : res.output;
    refreshAll();
  } else if (str === "t") {
    if (state.projects.length > 0) {
      const proj = state.projects[state.projectCursor];
      const res = runGoEngine(["teleport", "send", "--key", "pfk_default_tui_key"], fromWslPath(proj.path));
      state.statusMsg = res.ok ? "🚀 Cápsula Teleport cifrada creada con éxito." : res.output;
      refreshAll();
    }
  } else if (str === "x" && state.teleports.length > 0) {
    const tp = state.teleports[state.teleportCursor];
    const res = runGoEngine(["teleport", "revoke", tp.code], state.cwd);
    state.statusMsg = res.ok ? `🗑 Cápsula Teleport ${tp.code} revocada.` : res.output;
    refreshAll();
  }
}

function handleFeaturesAuditKeys(str, key) {
  if (key.name === "up" || str === "k") {
    if (state.featCursor > 0) state.featCursor--;
  } else if (key.name === "down" || str === "j") {
    if (state.featCursor + 1 < state.features.length) state.featCursor++;
  } else if (key.name === "space" || key.name === "return" || key.name === "enter" || str === " ") {
    const f = state.features[state.featCursor];
    if (f) toggleFeatureByKey(f.key);
  }
}

function toggleFeatureByKey(featKey) {
  const nextState = !state.config.features[featKey];
  state.config.features[featKey] = nextState;
  writeJSONAtomic(path.join(state.vaultDir, "config.json"), state.config);
  appendAudit(state.vaultDir, "FEATURE_TOGGLED", `${featKey}=${nextState} (via TUI)`);
  state.statusMsg = `⚙ Módulo '${featKey}' cambiado a ${nextState ? "[ON]" : "[OFF]"}.`;
  refreshAll();
}

function cleanupAndExit() {
  if (process.stdin.isTTY) {
    process.stdin.setRawMode(false);
  }
  process.stdout.write(CSI + "?25h" + C.reset + "\n");
  process.exit(0);
}

// ============================================================================
// Entrypoint
// ============================================================================

refreshAll();

if (process.argv.includes("--once")) {
  render();
  process.stdout.write("\n");
  process.exit(0);
}

readline.emitKeypressEvents(process.stdin);
if (process.stdin.isTTY) {
  process.stdin.setRawMode(true);
}
process.stdin.resume();
process.stdout.write(CSI + "?25l"); // Hide cursor
render();

process.stdin.on("keypress", (str, key) => {
  handleKey(str, key);
});
