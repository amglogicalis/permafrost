@echo off
setlocal
set "WIN_VAULT=/mnt/c/Users/%USERNAME%/.permafrost"
if not "%PFROST_VAULT_DIR%"=="" set "WIN_VAULT=%PFROST_VAULT_DIR%"
wsl.exe -e bash -c "export PFROST_VAULT_DIR='%WIN_VAULT%'; exec \"$HOME/.local/bin/pfrost\" \"$@\"" -- %*
