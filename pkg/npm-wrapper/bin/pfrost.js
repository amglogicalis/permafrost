#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");
const { spawnSync } = require("child_process");

const isWin = process.platform === "win32";
const exeName = isWin ? "pfrost.exe" : "pfrost";

const candidates = [
  path.join(__dirname, "..", "vendor", exeName),
  path.join(__dirname, "..", "..", "..", "pfrost.cmd"),
  exeName
];

let targetBin = exeName;
for (const c of candidates) {
  if (fs.existsSync(c)) {
    targetBin = c;
    break;
  }
}

const args = process.argv.slice(2);
let result;
if (isWin && targetBin.toLowerCase().endsWith(".cmd")) {
  result = spawnSync("cmd.exe", ["/d", "/s", "/c", targetBin, ...args], {
    stdio: "inherit"
  });
} else {
  result = spawnSync(targetBin, args, {
    stdio: "inherit"
  });
}

process.exit(result.status ?? 0);
