@echo off
setlocal EnableExtensions

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "STATE_DIR=%ROOT%"
if exist "%ROOT%\..\..\fengshen-engine.exe" for %%I in ("%ROOT%\..\..") do set "STATE_DIR=%%~fI"
if defined FENGSHEN_STATE_DIR set "STATE_DIR=%FENGSHEN_STATE_DIR%"
set "PIDFILE=%STATE_DIR%\model_center.pid"
if not exist "%PIDFILE%" set "PIDFILE=%USERPROFILE%\.fengshen\model_center.pid"

set "CENTER_PID="
if exist "%PIDFILE%" set /p "CENTER_PID="<"%PIDFILE%"
if not defined CENTER_PID (
    echo [info] not running: pid file missing
    if exist "%PIDFILE%" del /q "%PIDFILE%" >nul 2>nul
    exit /b 0
)

call :is_running "%CENTER_PID%"
if errorlevel 1 (
    echo [info] not running: stale pid %CENTER_PID%
    del /q "%PIDFILE%" >nul 2>nul
    exit /b 0
)

echo [info] stopping center pid %CENTER_PID%
taskkill /T /F /PID %CENTER_PID% >nul 2>nul
if errorlevel 1 (
    call :is_running "%CENTER_PID%"
    if errorlevel 1 (
        del /q "%PIDFILE%" >nul 2>nul
        echo [info] already stopped
        exit /b 0
    )
    echo [error] failed to stop pid %CENTER_PID%
    exit /b 1
)
del /q "%PIDFILE%" >nul 2>nul
echo [info] stopped
exit /b 0

:is_running
tasklist /FI "PID eq %~1" /FI "IMAGENAME eq fengshen-model-center.exe" /NH 2>nul | findstr /i "fengshen-model-center.exe" >nul
exit /b %ERRORLEVEL%