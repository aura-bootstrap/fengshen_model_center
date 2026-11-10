@echo off
setlocal EnableExtensions

set "PIDFILE=%USERPROFILE%\.fengshen\model_center.pid"

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