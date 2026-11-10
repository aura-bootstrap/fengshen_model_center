@echo off
setlocal EnableExtensions EnableDelayedExpansion

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "BUILD_ROOT=%ROOT%\build"
if not exist "%BUILD_ROOT%" mkdir "%BUILD_ROOT%"

set "CENTER_SCRIPT=%ROOT%\external\model-center\build.ps1"
set "WHISPER_BATCH_SCRIPT=%ROOT%\external\whisper\batch\build.ps1"
set "WHISPER_STREAM_SCRIPT=%ROOT%\external\whisper\stream\build.ps1"
set "LLAMA_SCRIPT=%ROOT%\external\llama\runtime\build.ps1"
set "INSTALL_BAT=%ROOT%\scripts\install.bat"

set "PS="
where pwsh.exe >nul 2>nul && set "PS=pwsh.exe"
if not defined PS where powershell.exe >nul 2>nul && set "PS=powershell.exe"
if not defined PS (
    echo 未找到 PowerShell，请安装 PowerShell 7 或启用 Windows PowerShell。
    pause
    exit /b 1
)

set "PRESET=%~1"
set "VERSION=dev-local"
set "LLAMA_PROFILE=baseline"
set "WHISPER_DEPS=%FENGSHEN_WHISPER_DEPS%"
set "LLAMA_ARCHIVE=%FENGSHEN_LLAMA_SOURCE_ARCHIVE%"
set "FREEZE_INPUTS=%FENGSHEN_FREEZE_INPUTS%"
set "FREEZE_LOCK=%FENGSHEN_FREEZE_INPUT_LOCK%"

:menu
set "CHOICE="
cls
echo.
echo ============================================================
echo   峰神模型中心 · 构建与安装
echo ============================================================
echo   仓库根目录 : %ROOT%
echo   构建产物   : %BUILD_ROOT%
echo   构建版本   : %VERSION%
echo   Whisper依赖: %WHISPER_DEPS%
echo   Llama 归档 : %LLAMA_ARCHIVE%
echo   冻结输入   : %FREEZE_INPUTS%
echo   Llama 配置 : %LLAMA_PROFILE%
echo.
echo   1. 构建全部（中心 + shim + 可用的原生/冻结组件）
echo   2. 仅构建中心与 shim
echo   3. 构建 whisper-batch
echo   4. 构建 whisper-stream
echo   5. 构建 llama-runtime
echo   6. 冻结 Demucs / Seed-VC
echo   7. 设置依赖路径与版本
echo   8. 清理 build 目录
echo   9. 安装到 bin（平铺覆盖）
echo   0. 退出
echo.
if defined PRESET set "CHOICE=%PRESET%"
if defined PRESET set "PRESET="
if not defined CHOICE goto ask_choice
goto choice_ready
:ask_choice
set "CHOICE=__EOF__"
set /p "CHOICE=请选择 [0-9]: "
if "%CHOICE%"=="__EOF__" goto end
:choice_ready
if not defined CHOICE goto menu
if "%CHOICE%"=="1" goto build_all
if "%CHOICE%"=="2" goto build_center
if "%CHOICE%"=="3" goto build_whisper_batch
if "%CHOICE%"=="4" goto build_whisper_stream
if "%CHOICE%"=="5" goto build_llama
if "%CHOICE%"=="6" goto build_timbre
if "%CHOICE%"=="7" goto settings
if "%CHOICE%"=="8" goto clean
if "%CHOICE%"=="9" goto install_menu
if "%CHOICE%"=="0" goto end
echo 无效选择: %CHOICE%
pause
goto menu

:build_all
call :ensure_whisper_deps
call :ensure_llama_archive
call :ensure_freeze
call :new_out all
set "ARGS="
if defined WHISPER_DEPS set "ARGS=%ARGS% -BuildWhisperBatch -BuildWhisperBatchDependencyRoot "%WHISPER_DEPS%" -BuildWhisperStream -WhisperStreamDependencyRoot "%WHISPER_DEPS%""
if defined LLAMA_ARCHIVE set "ARGS=%ARGS% -BuildLlamaRuntime -LlamaSourceArchive "%LLAMA_ARCHIVE%" -LlamaProfile "%LLAMA_PROFILE%""
if defined FREEZE_INPUTS if defined FREEZE_LOCK set "ARGS=%ARGS% -FreezeDemucs -FreezeSeedVC -Inputs "%FREEZE_INPUTS%" -InputLock "%FREEZE_LOCK%""
echo.
echo 输出目录: %OUT%
if not defined WHISPER_DEPS echo [跳过] whisper 原生组件：未设置依赖目录
if not defined LLAMA_ARCHIVE echo [跳过] llama-runtime：未设置源码归档
if not defined FREEZE_INPUTS echo [跳过] Demucs/Seed-VC 冻结：未设置冻结输入
call :run_center %ARGS%
set "RC=%ERRORLEVEL%"
call :finish %RC%
goto menu
:build_center
call :new_out center
call :run_center
set "RC=%ERRORLEVEL%"
call :finish %RC%
goto menu

:build_whisper_batch
call :ensure_whisper_deps
if not defined WHISPER_DEPS (
    echo [跳过] whisper-batch：未设置 Whisper 依赖目录。
    pause
    goto menu
)
call :new_out whisper-batch
echo.
echo 输出目录: %OUT%
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%WHISPER_BATCH_SCRIPT%" -DependencyRoot "%WHISPER_DEPS%" -BuildDirectory "%OUT%"
call :finish %ERRORLEVEL%
goto menu

:build_whisper_stream
call :ensure_whisper_deps
if not defined WHISPER_DEPS (
    echo [跳过] whisper-stream：未设置 Whisper 依赖目录。
    pause
    goto menu
)
call :new_out whisper-stream
echo.
echo 输出目录: %OUT%
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%WHISPER_STREAM_SCRIPT%" -DependencyRoot "%WHISPER_DEPS%" -BuildDirectory "%OUT%"
call :finish %ERRORLEVEL%
goto menu

:build_llama
call :ensure_llama_archive
if not defined LLAMA_ARCHIVE (
    echo [跳过] llama-runtime：未设置源码归档。
    pause
    goto menu
)
call :new_out llama-runtime
echo.
echo 输出目录: %OUT%
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%LLAMA_SCRIPT%" -SourceArchive "%LLAMA_ARCHIVE%" -BuildDirectory "%OUT%" -Profile "%LLAMA_PROFILE%"
call :finish %ERRORLEVEL%
goto menu

:build_timbre
call :ensure_freeze
if not defined FREEZE_INPUTS (
    echo [跳过] Demucs/Seed-VC：未设置冻结输入。
    pause
    goto menu
)
call :new_out timbre
echo.
echo 输出目录: %OUT%
call :run_center -FreezeDemucs -FreezeSeedVC -Inputs "%FREEZE_INPUTS%" -InputLock "%FREEZE_LOCK%"
set "RC=%ERRORLEVEL%"
call :finish %RC%
goto menu

:settings
echo.
set "VERSION="
set /p "VERSION=构建版本 [dev-local]: "
if not defined VERSION set "VERSION=dev-local"
set "WHISPER_DEPS="
set /p "WHISPER_DEPS=Whisper 依赖目录（留空清除）: "
set "LLAMA_ARCHIVE="
set /p "LLAMA_ARCHIVE=llama 源码归档（留空清除）: "
set "FREEZE_INPUTS="
set /p "FREEZE_INPUTS=Python 冻结输入目录（留空清除）: "
set "FREEZE_LOCK="
if defined FREEZE_INPUTS set /p "FREEZE_LOCK=输入锁文件: "
set "LLAMA_PROFILE="
set /p "LLAMA_PROFILE=llama 配置 baseline/avx2 [baseline]: "
if not defined LLAMA_PROFILE set "LLAMA_PROFILE=baseline"
if /i not "%LLAMA_PROFILE%"=="baseline" if /i not "%LLAMA_PROFILE%"=="avx2" set "LLAMA_PROFILE=baseline"
echo 已更新本次会话的构建设置。
pause
goto menu

:clean
echo.
echo 将删除 %BUILD_ROOT% 下的所有构建产物。
set "CONFIRM="
set /p "CONFIRM=确认清理? [y/N]: "
if /i not "%CONFIRM%"=="y" goto menu
if exist "%BUILD_ROOT%" rmdir /s /q "%BUILD_ROOT%"
mkdir "%BUILD_ROOT%" 2>nul
echo [完成] build 目录已清理。
pause
goto menu

:ensure_whisper_deps
if defined WHISPER_DEPS exit /b 0
set /p "WHISPER_DEPS=Whisper 依赖目录（含 ggml-org-whisper.cpp-4979e04，留空跳过）: "
exit /b 0

:ensure_llama_archive
if defined LLAMA_ARCHIVE exit /b 0
set /p "LLAMA_ARCHIVE=llama 源码归档（留空跳过）: "
exit /b 0

:ensure_freeze
if defined FREEZE_INPUTS if defined FREEZE_LOCK exit /b 0
set /p "FREEZE_INPUTS=Python 冻结离线输入目录（留空跳过）: "
if not defined FREEZE_INPUTS (
    set "FREEZE_LOCK="
    exit /b 0
)
set /p "FREEZE_LOCK=输入锁文件路径: "
if not defined FREEZE_LOCK set "FREEZE_INPUTS="
exit /b 0

:new_out
for /f %%I in ('%PS% -NoProfile -Command "Get-Date -Format yyyyMMdd-HHmmss-fff"') do set "STAMP=%%I"
if not defined STAMP set "STAMP=%RANDOM%"
set "OUT=%BUILD_ROOT%\%~1-%STAMP%"
exit /b 0

:run_center
echo.
echo [构建] 中心与交付...
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%CENTER_SCRIPT%" -Output "%OUT%" -Version "%VERSION%" %*
exit /b %ERRORLEVEL%

:install_menu
set "CHOICE="
cls
echo.
echo ============================================================
echo   安装到 bin（平铺覆盖）
echo ============================================================
echo   仓库根目录 : %ROOT%
echo   bin 目录   : %ROOT%\bin
echo   安装来源   : build 下各部分的最近一次完整构建
echo   同名文件直接覆盖，不创建子目录。
echo.
echo   1. 安装全部（需已构建对应部分）
echo   2. 安装中心与 shim
echo   3. 安装 whisper-batch
echo   4. 安装 whisper-stream
echo   5. 安装 llama-runtime
echo   6. 安装 Demucs / Seed-VC
echo   0. 返回主菜单
echo.
set "CHOICE=__EOF__"
set /p "CHOICE=请选择 [0-6]: "
if "%CHOICE%"=="__EOF__" goto end
if not defined CHOICE goto install_menu
if "%CHOICE%"=="1" goto install_all
if "%CHOICE%"=="2" goto install_center
if "%CHOICE%"=="3" goto install_whisper_batch
if "%CHOICE%"=="4" goto install_whisper_stream
if "%CHOICE%"=="5" goto install_llama
if "%CHOICE%"=="6" goto install_timbre
if "%CHOICE%"=="0" goto menu
echo 无效选择: %CHOICE%
pause
goto install_menu

:install_all
call :install_part all
goto install_menu

:install_center
call :install_part center
goto install_menu

:install_whisper_batch
call :install_part whisper-batch
goto install_menu

:install_whisper_stream
call :install_part whisper-stream
goto install_menu

:install_llama
call :install_part llama-runtime
goto install_menu

:install_timbre
call :install_part timbre
goto install_menu

:install_part
echo.
echo [安装] %~1 到 %ROOT%\bin
call "%INSTALL_BAT%" "%~1"
set "RC=%ERRORLEVEL%"
echo.
if "%RC%"=="0" (
    echo [完成] 已平铺安装到 %ROOT%\bin
) else (
    echo [失败] 安装退出码 %RC%，请查看上方日志。
)
pause
exit /b 0

:finish
if "%~1"=="0" (
    echo.
    echo [完成] 产物目录: %OUT%
    echo 安装到 bin: 主菜单 9
) else (
    echo.
    echo [失败] 退出码 %~1，请查看上方日志。
)
echo.
pause
exit /b 0

:end
echo 退出。
endlocal
exit /b 0
