@echo off
setlocal EnableExtensions EnableDelayedExpansion

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "WORK=%ROOT%\build\full-test"
set "LOG=%WORK%\go-test.log"
set "PROFILE=%WORK%\coverage.out"
set "FIRSTPARTY=%WORK%\coverage-firstparty.out"
set "FUNC=%WORK%\coverage-func.txt"
set "HTML=%WORK%\coverage.html"

set "MIN_FIRST=70.0"
set "MIN_PKG=40.0"
set "SKIP_PROCESS="
set "SKIP_CONTRACT="

:parse_args
if "%~1"=="" goto args_done
if /i "%~1"=="-MinFirstPartyCoverage" goto arg_min_first
if /i "%~1"=="-MinPackageCoverage" goto arg_min_pkg
if /i "%~1"=="-SkipProcess" goto arg_skip_process
if /i "%~1"=="-SkipBuildContract" goto arg_skip_contract
echo [error] unknown argument: %~1
echo usage: %~nx0 [-MinFirstPartyCoverage N] [-MinPackageCoverage N] [-SkipProcess] [-SkipBuildContract]
exit /b 1

:arg_min_first
if "%~2"=="" (
    echo [error] missing value for -MinFirstPartyCoverage
    exit /b 1
)
set "MIN_FIRST=%~2"
shift
shift
goto parse_args

:arg_min_pkg
if "%~2"=="" (
    echo [error] missing value for -MinPackageCoverage
    exit /b 1
)
set "MIN_PKG=%~2"
shift
shift
goto parse_args

:arg_skip_process
set "SKIP_PROCESS=1"
shift
goto parse_args

:arg_skip_contract
set "SKIP_CONTRACT=1"
shift
goto parse_args

:args_done
set "PS="
where pwsh.exe >nul 2>nul && set "PS=pwsh.exe"
if not defined PS where powershell.exe >nul 2>nul && set "PS=powershell.exe"
if not defined PS (
    echo [error] PowerShell not found.
    exit /b 1
)
set "GOTOOLCHAIN=local"
if not exist "%WORK%" mkdir "%WORK%"
pushd "%ROOT%"

echo.
echo === go vet ===
go vet ./...
if errorlevel 1 (
    echo [error] go vet failed
    popd
    exit /b 1
)

echo.
echo === go test with coverage (no cache) ===
go test -count=1 -covermode=atomic -coverprofile="%PROFILE%" ./... > "%LOG%" 2>&1
set "TEST_RC=%ERRORLEVEL%"
type "%LOG%"
if not "%TEST_RC%"=="0" (
    echo [error] go test failed with exit code %TEST_RC%
    popd
    exit /b 1
)

echo.
echo === first-party coverage ===
findstr /v /i /c:"internal/upstream/" "%PROFILE%" > "%FIRSTPARTY%"
go tool cover -func="%FIRSTPARTY%" > "%FUNC%" 2>&1
if errorlevel 1 (
    type "%FUNC%"
    echo [error] go tool cover failed
    popd
    exit /b 1
)
set "COV_RAW="
for /f "usebackq tokens=3" %%A in ("%FUNC%") do set "COV_RAW=%%A"
if not defined COV_RAW (
    type "%FUNC%"
    echo [error] cannot parse coverage total
    popd
    exit /b 1
)
set "COV=%COV_RAW:~0,-1%"
call :to_tenths "%COV%" COV_T
call :to_tenths "%MIN_FIRST%" MIN_FIRST_T
echo first-party coverage: %COV% percent
if %COV_T% LSS %MIN_FIRST_T% (
    echo [error] first-party coverage %COV% percent below gate %MIN_FIRST% percent
    popd
    exit /b 1
)

echo.
echo === per-package coverage gate ===
set "BELOW="
set "BELOW_LIST="
call :to_tenths "%MIN_PKG%" MIN_PKG_T
for /f "usebackq tokens=1,2,4,5" %%A in ("%LOG%") do (
    if /i "%%A"=="ok" if /i "%%C"=="coverage:" (
        set "PKG=%%B"
        set "PKGV=%%D"
        set "PKGV=!PKGV:~0,-1!"
        call :to_tenths "!PKGV!" PKG_T
        echo !PKGV!%%  !PKG!
        if !PKG_T! LSS %MIN_PKG_T% (
            set "BELOW=1"
            set "BELOW_LIST=!BELOW_LIST! !PKG!=!PKGV!"
        )
    )
)
if defined BELOW (
    echo [error] packages below %MIN_PKG% percent:%BELOW_LIST%
    popd
    exit /b 1
)

echo.
echo === coverage HTML ===
go tool cover -html="%FIRSTPARTY%" -o "%HTML%"
if errorlevel 1 (
    echo [error] coverage html failed
    popd
    exit /b 1
)
echo coverage report: %HTML%

if defined SKIP_CONTRACT goto contract_done
echo.
echo === build contract ===
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%ROOT%\external\model-center\build_test.ps1"
if errorlevel 1 (
    echo [error] build contract failed
    popd
    exit /b 1
)
:contract_done

if defined SKIP_PROCESS goto process_done

echo.
echo === installed bin binaries ===
if not exist "%ROOT%\bin\fengshen-model-center.exe" (
    echo [error] missing bin\fengshen-model-center.exe; run scripts\install.bat center first
    popd
    exit /b 1
)
if not exist "%ROOT%\bin\fengshen-shim.exe" (
    echo [error] missing bin\fengshen-shim.exe; run scripts\install.bat center first
    popd
    exit /b 1
)

set "STATE_DIR=%USERPROFILE%\.fengshen"
set "PIDFILE=%STATE_DIR%\model_center.pid"
set "BASE=http://127.0.0.1:18120"
set "STATE_FILE=%WORK%\state.json"
set "REPORT_FILE=%WORK%\report.json"
set "COMPONENTS_FILE=%WORK%\components.json"

set "CENTER_PID="
if exist "%PIDFILE%" set /p "CENTER_PID="<"%PIDFILE%"
if defined CENTER_PID call :pid_alive "%CENTER_PID%"
if not errorlevel 1 goto center_ready

echo.
echo === start center via run.bat ===
call "%ROOT%\scripts\run.bat"
if errorlevel 1 (
    echo [error] run.bat failed
    popd
    exit /b 1
)
set "CENTER_PID="
if exist "%PIDFILE%" set /p "CENTER_PID="<"%PIDFILE%"
if not defined CENTER_PID (
    echo [error] pid file missing after run.bat: %PIDFILE%
    popd
    exit /b 1
)

:center_ready
call :pid_alive "%CENTER_PID%"
if errorlevel 1 (
    echo [error] center process not running: pid %CENTER_PID%
    popd
    exit /b 1
)
echo [info] testing center pid %CENTER_PID%

echo.
echo === run.bat process health ===
set "READY="
for /l %%I in (1,1,40) do (
    if not defined READY (
        curl.exe -s -f -o NUL --max-time 2 "%BASE%/api/state" >nul 2>nul
        if not errorlevel 1 set "READY=1"
        if not defined READY ping -n 2 127.0.0.1 >nul
    )
)
if not defined READY (
    echo [error] center API not ready: %BASE%/api/state
    popd
    exit /b 1
)
curl.exe -s -f -o "%STATE_FILE%" --max-time 5 "%BASE%/api/state"
if errorlevel 1 (
    echo [error] GET /api/state failed
    popd
    exit /b 1
)
curl.exe -s -f -o "%REPORT_FILE%" --max-time 5 "%BASE%/api/report"
if errorlevel 1 (
    echo [error] GET /api/report failed
    popd
    exit /b 1
)
curl.exe -s -f -o "%COMPONENTS_FILE%" --max-time 5 "%BASE%/api/components"
if errorlevel 1 (
    echo [error] GET /api/components failed
    popd
    exit /b 1
)
findstr /c:"catalog" "%STATE_FILE%" >nul
if errorlevel 1 (
    echo [error] state payload missing catalog
    popd
    exit /b 1
)
findstr /c:"summary" "%REPORT_FILE%" >nul
if errorlevel 1 (
    echo [error] report payload missing summary
    popd
    exit /b 1
)
findstr /c:"component_id" "%COMPONENTS_FILE%" >nul
if errorlevel 1 (
    echo [error] components payload missing component_id
    popd
    exit /b 1
)
call :pid_alive "%CENTER_PID%"
if errorlevel 1 (
    echo [error] center exited during process test
    popd
    exit /b 1
)
echo PASS: run.bat process health (pid %CENTER_PID%, state/report/components)

:process_done
echo.
echo PASS: test completed (first-party coverage %COV% percent, package gate %MIN_PKG% percent)
echo artifacts: %WORK%
popd
exit /b 0

:to_tenths
set "TVAL=%~1"
set "TINT="
set "TFRAC="
for /f "tokens=1,2 delims=." %%A in ("%TVAL%") do (
    set "TINT=%%A"
    set "TFRAC=%%B"
)
if not defined TINT set "TINT=0"
if not defined TFRAC set "TFRAC=0"
set "TFRAC=%TFRAC:~0,1%"
set /a "%~2=TINT*10+TFRAC"
exit /b 0

:pid_alive
tasklist /FI "PID eq %~1" /FI "IMAGENAME eq fengshen-model-center.exe" /NH 2>nul | findstr /i "fengshen-model-center.exe" >nul
exit /b %ERRORLEVEL%