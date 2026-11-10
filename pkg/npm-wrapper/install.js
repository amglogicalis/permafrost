#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");
const https = require("https");
const { execSync } = require("child_process");

const pkg = require("./package.json");
const version = pkg.version;
const repo = "amglogicalis/permafrost";

function getPlatformArch() {
  const platformMap = {
    linux: "linux",
    darwin: "darwin",
    win32: "windows"
  };
  const archMap = {
    x64: "amd64",
    arm64: "arm64"
  };
  const osName = platformMap[process.platform];
  const archName = archMap[process.arch];
  if (!osName || !archName) {
    throw new Error(`Unsupported platform/arch: ${process.platform}/${process.arch}`);
  }
  return { osName, archName };
}

function downloadFile(url, dest) {
  return new Promise((resolve, reject) => {
    https.get(url, (res) => {
      if (res.statusCode === 301 || res.statusCode === 302) {
        return downloadFile(res.headers.location, dest).then(resolve).catch(reject);
      }
      if (res.statusCode !== 200) {
        return reject(new Error(`HTTP ${res.statusCode} for ${url}`));
      }
      const file = fs.createWriteStream(dest);
      res.pipe(file);
      file.on("finish", () => file.close(resolve));
    }).on("error", reject);
  });
}

async function main() {
  const binDir = path.join(__dirname, "vendor");
  fs.mkdirSync(binDir, { recursive: true });

  const { osName, archName } = getPlatformArch();
  const ext = osName === "windows" ? "zip" : "tar.gz";
  const archiveName = `pfrost_${version}_${osName}_${archName}.${ext}`;
  const url = `https://github.com/${repo}/releases/download/v${version}/${archiveName}`;
  const archivePath = path.join(binDir, archiveName);

  try {
    console.log(`[pfrost] Checking native binary (${osName}/${archName})...`);
    await downloadFile(url, archivePath);
    if (osName === "windows") {
      execSync(`tar -xf "${archivePath}" -C "${binDir}"`);
    } else {
      execSync(`tar -xzf "${archivePath}" -C "${binDir}"`);
      fs.chmodSync(path.join(binDir, "pfrost"), 0o755);
    }
    fs.unlinkSync(archivePath);
    console.log("[pfrost] Native Go binary installed successfully.");
  } catch (err) {
    console.log("[pfrost] Prebuilt release not yet downloaded; will use system pfrost binary if available.");
  }
}

main();
