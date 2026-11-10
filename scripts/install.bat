@echo off
setlocal EnableExtensions EnableDelayedExpansion

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "BUILD_ROOT=%ROOT%\build"
set "BIN=%ROOT%\bin"

set "PART=%~1"
if not defined PART set "PART=all"
set "BUILD_OUTPUT=%~2"

set "VALID="
for %%P in (all center whisper-batch whisper-stream llama-runtime timbre) do if /i "%PART%"=="%%P" set "VALID=1"
if not defined VALID (
    echo [error] unknown part: %PART%
    echo usage: %~nx0 [all^|center^|whisper-batch^|whisper-stream^|llama-runtime^|timbre] [build-output]
    exit /b 2
)

if not defined BUILD_OUTPUT goto use_build_root
if "%BUILD_OUTPUT:~1,1%"==":" goto build_output_absolute
set "BUILD_OUTPUT=%ROOT%\%BUILD_OUTPUT%"
:build_output_absolute
for %%I in ("%BUILD_OUTPUT%") do set "BUILD_OUTPUT=%%~fI"
if not exist "%BUILD_OUTPUT%\" (
    echo [error] build output not found: %BUILD_OUTPUT%
    exit /b 1
)
set "SOURCE_ROOT=%BUILD_OUTPUT%"
goto source_ready

:use_build_root
if not exist "%BUILD_ROOT%\" (
    echo [error] build directory not found: %BUILD_ROOT%
    exit /b 1
)
set "SOURCE_ROOT=%BUILD_ROOT%"

:source_ready
set "INSTALLED="
set "MISSING="
set "INSTALL_ERROR="
set "BIN_READY="
set "FOUND="

if /i "%PART%"=="all" goto install_all
call :process_part "%PART%"
goto install_summary

:install_all
for %%P in (center whisper-batch whisper-stream llama-runtime timbre) do call :process_part "%%P"

:install_summary
if defined MISSING for %%M in (%MISSING%) do echo [skip] %%M : no complete build found under %SOURCE_ROOT%
if not defined INSTALLED (
    echo [error] no complete build found for: %PART% under %SOURCE_ROOT%
    exit /b 1
)
echo bin installed: %BIN%
if defined MISSING exit /b 1
if defined INSTALL_ERROR exit /b 1
exit /b 0

:process_part
set "CURRENT_PART=%~1"
call :find_and_install "%CURRENT_PART%"
if defined FOUND (
    set "INSTALLED=!INSTALLED! !CURRENT_PART!"
    echo [part] !CURRENT_PART! installed
) else (
    set "MISSING=!MISSING! !CURRENT_PART!"
)
exit /b 0

:find_and_install
set "PART_TO_FIND=%~1"
set "FOUND="
if defined BUILD_OUTPUT goto find_single
for /f "delims=" %%D in ('dir /b /ad /o-d "%BUILD_ROOT%" 2^>nul') do (
    if not defined FOUND call :try_part "%BUILD_ROOT%\%%D" "%PART_TO_FIND%"
)
exit /b 0

:find_single
call :try_part "%BUILD_OUTPUT%" "%PART_TO_FIND%"
exit /b 0

:try_part
set "CANDIDATE=%~1"
set "PART_NAME=%~2"
if /i "%PART_NAME%"=="center" goto try_center
if /i "%PART_NAME%"=="timbre" goto try_timbre
call :try_native "%CANDIDATE%" "%PART_NAME%"
exit /b 0

:try_center
set "CENTER_DIR=%CANDIDATE%\model-center"
if not exist "%CENTER_DIR%\fengshen-model-center.exe" exit /b 0
if not exist "%CENTER_DIR%\fengshen-shim.exe" exit /b 0
call :prepare_bin
call :copy_one "%CENTER_DIR%\fengshen-model-center.exe" "fengshen-model-center.exe"
call :copy_one "%CENTER_DIR%\fengshen-shim.exe" "fengshen-shim.exe"
set "FOUND=1"
exit /b 0

:try_native
call :resolve_component "%~1" "%~2"
if not defined COMPONENT_DIR exit /b 0
call :prepare_bin
call :copy_one "%COMPONENT_DIR%\%~2.exe" "%~2.exe"
if /i "%~2"=="llama-runtime" call :copy_llama_server "%COMPONENT_DIR%"
set "FOUND=1"
exit /b 0

:try_timbre
set "DIR_DEMUCS="
set "DIR_SEEDVC="
call :resolve_component "%CANDIDATE%" "demucs-runner"
set "DIR_DEMUCS=%COMPONENT_DIR%"
call :resolve_component "%CANDIDATE%" "seedvc-runner"
set "DIR_SEEDVC=%COMPONENT_DIR%"
if not defined DIR_DEMUCS if not defined DIR_SEEDVC exit /b 0
if defined DIR_DEMUCS call :install_timbre_one "demucs-runner" "%DIR_DEMUCS%"
if defined DIR_SEEDVC call :install_timbre_one "seedvc-runner" "%DIR_SEEDVC%"
if not defined DIR_DEMUCS echo [warn] timbre: demucs-runner missing
if not defined DIR_SEEDVC echo [warn] timbre: seedvc-runner missing
set "FOUND=1"
exit /b 0

:install_timbre_one
call :prepare_bin
call :copy_one "%~2\%~1.exe" "%~1.exe"
echo [warn] %~1: pyinstaller-onedir; only the entry exe is installed flat, its support files stay in build output
exit /b 0

:copy_llama_server
if exist "%~1\internal\llama-server.exe" (
    call :copy_one "%~1\internal\llama-server.exe" "llama-server.exe"
) else (
    echo [warn] llama-runtime: internal\llama-server.exe missing; llama-runtime cannot start a server
)
exit /b 0

:resolve_component
set "COMPONENT_DIR="
for %%P in ("%~1\components\%~2" "%~1\model-center\components\%~2") do (
    if exist "%%~P" (
        for /f "delims=" %%V in ('dir /b /ad /o-d "%%~P" 2^>nul') do (
            if not defined COMPONENT_DIR if exist "%%~P\%%V\%~2.exe" set "COMPONENT_DIR=%%~P\%%V"
        )
    )
)
exit /b 0

:prepare_bin
if "%BIN_READY%"=="1" exit /b 0
if not exist "%BIN%" mkdir "%BIN%"
if exist "%BIN%\components" rmdir /s /q "%BIN%\components"
if exist "%BIN%\model-center" rmdir /s /q "%BIN%\model-center"
if exist "%BIN%\engines" rmdir /s /q "%BIN%\engines"
if exist "%BIN%\delivery.json" del /q "%BIN%\delivery.json"
if exist "%BIN%\run-model-center.bat" del /q "%BIN%\run-model-center.bat"
set "BIN_READY=1"
exit /b 0

:copy_one
set "TARGET=%BIN%\%~2"
if exist "%TARGET%" (
    echo [overwrite] %~2
    attrib -r "%TARGET%" >nul 2>nul
) else (
    echo [install]   %~2
)
copy /y "%~1" "%TARGET%" >nul
if errorlevel 1 (
    echo [error] copy failed: %~1
    set "INSTALL_ERROR=1"
)
exit /b 0