@echo off
setlocal EnableExtensions

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "BIN=%ROOT%\bin"
set "CENTER=%BIN%\fengshen-model-center.exe"
set "STATE_DIR=%ROOT%"
if exist "%ROOT%\..\..\fengshen-engine.exe" for %%I in ("%ROOT%\..\..") do set "STATE_DIR=%%~fI"
if defined FENGSHEN_STATE_DIR set "STATE_DIR=%FENGSHEN_STATE_DIR%"
set "LOGDIR=%STATE_DIR%\logs"
if not exist "%LOGDIR%" mkdir "%LOGDIR%"
set "PIDFILE=%STATE_DIR%\model_center.pid"
set "OUTLOG=%LOGDIR%\model_center.out.log"
set "ERRLOG=%LOGDIR%\model_center.err.log"

rem Migrate pre-existing user-profile state (best-effort).
set "LEGACY=%USERPROFILE%\.fengshen"
call :migrate "%LEGACY%\model_center.pid" "%PIDFILE%"
call :migrate "%LEGACY%\model_center.out.log" "%OUTLOG%"
call :migrate "%LEGACY%\model_center.err.log" "%ERRLOG%"
rmdir "%LEGACY%" 2>nul

if not exist "%CENTER%" (
    echo [error] missing %CENTER%
    echo run scripts\install.bat center to install it first.
    exit /b 1
)

set "MODEL_ROOT=%~1"
if not defined MODEL_ROOT set "MODEL_ROOT=W:\model_root"
set "LISTEN=%~2"
if not defined LISTEN set "LISTEN=127.0.0.1:18120"

set "CENTER_PID="
if exist "%PIDFILE%" set /p "CENTER_PID="<"%PIDFILE%"
if not defined CENTER_PID goto start_center
call :is_running "%CENTER_PID%"
if errorlevel 1 goto start_center
echo [info] already running: pid %CENTER_PID%
echo [info] pid file: %PIDFILE%
exit /b 0

:start_center
if exist "%PIDFILE%" del /q "%PIDFILE%" >nul 2>nul

set "MANIFEST_FOUND="
for %%C in (user dev admin) do if exist "%USERPROFILE%\.fengshen\slicer\%%C\model_manifest.json" set "MANIFEST_FOUND=1"
if exist "%STATE_DIR%\model_manifest.json" set "MANIFEST_FOUND=1"
if defined MANIFEST_FOUND (
    echo [info] user manifest present
) else (
    if not exist "%MODEL_ROOT%\" (
        echo [error] user manifest not found and model root not found: %MODEL_ROOT%
        echo usage: %~nx0 [model_root] [listen]
        exit /b 1
    )
    echo [info] generating user manifest from %MODEL_ROOT%
    "%CENTER%" --write-model-manifest "%MODEL_ROOT%"
    if errorlevel 1 (
        echo [error] model manifest generation failed
        exit /b 1
    )
)

if not exist "%STATE_DIR%" mkdir "%STATE_DIR%"
set "FENGSHEN_CENTER=%CENTER%"
set "FENGSHEN_BIN=%BIN%"
set "FENGSHEN_LISTEN=%LISTEN%"
set "FENGSHEN_OUTLOG=%OUTLOG%"
set "FENGSHEN_ERRLOG=%ERRLOG%"
set "FENGSHEN_PIDFILE=%PIDFILE%"
powershell.exe -NoProfile -Command "$ErrorActionPreference='Stop'; $a = @('--root', $env:FENGSHEN_BIN, '--install-root', $env:FENGSHEN_BIN, '--root-kind', 'installed', '--listen', $env:FENGSHEN_LISTEN); if ($env:FENGSHEN_STATE_DIR) { $a += @('--state-dir', $env:FENGSHEN_STATE_DIR) }; $p = Start-Process -FilePath $env:FENGSHEN_CENTER -ArgumentList $a -WorkingDirectory $env:FENGSHEN_BIN -WindowStyle Hidden -RedirectStandardOutput $env:FENGSHEN_OUTLOG -RedirectStandardError $env:FENGSHEN_ERRLOG -PassThru; Set-Content -LiteralPath $env:FENGSHEN_PIDFILE -Value $p.Id -Encoding ascii" >nul 2>nul
set "CENTER_PID="
if exist "%PIDFILE%" set /p "CENTER_PID="<"%PIDFILE%"
if not defined CENTER_PID (
    echo [error] failed to start center
    exit /b 1
)
ping -n 2 127.0.0.1 >nul
call :is_running "%CENTER_PID%"
if errorlevel 1 (
    echo [error] center exited after start; see %ERRLOG%
    if exist "%ERRLOG%" type "%ERRLOG%"
    del /q "%PIDFILE%" >nul 2>nul
    exit /b 1
)
echo [info] center started: pid %CENTER_PID%
echo [info] pid file: %PIDFILE%
echo [info] listen: http://%LISTEN%
echo [info] logs: %OUTLOG%
echo [info] stop: taskkill /T /F /PID %CENTER_PID%
exit /b 0

:migrate
if not exist "%~1" exit /b 0
if exist "%~2" (del /q "%~1" >nul 2>nul) else (move /y "%~1" "%~2" >nul 2>nul)
exit /b 0

:is_running
tasklist /FI "PID eq %~1" /FI "IMAGENAME eq fengshen-model-center.exe" /NH 2>nul | findstr /i "fengshen-model-center.exe" >nul
exit /b %ERRORLEVEL%