@echo off
setlocal
set "WIN_VAULT=/mnt/c/Users/%USERNAME%/.permafrost"
if not "%PFROST_VAULT_DIR%"=="" set "WIN_VAULT=%PFROST_VAULT_DIR%"

if "%~1"=="" goto :launch_tui
if /I "%~1"=="ui" goto :launch_tui
if /I "%~1"=="tui" goto :launch_tui
if /I "%~1"=="menu" goto :launch_tui

wsl.exe -e bash -c "export PFROST_VAULT_DIR='%WIN_VAULT%'; exec \"$HOME/.local/bin/pfrost\" \"$@\"" -- %*
exit /b %ERRORLEVEL%

:launch_tui
if exist "%~dp0scripts\tui-win.js" (
    node "%~dp0scripts\tui-win.js"
    exit /b %ERRORLEVEL%
)
if exist "%USERPROFILE%\.permafrost\bin\tui-win.js" (
    node "%USERPROFILE%\.permafrost\bin\tui-win.js"
    exit /b %ERRORLEVEL%
)
node "c:\testing\permafrost\scripts\tui-win.js"
exit /b %ERRORLEVEL%
