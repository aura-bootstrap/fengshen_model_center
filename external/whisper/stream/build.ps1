param(
    [Parameter(Mandatory=$true)][string]$DependencyRoot,
    [Parameter(Mandatory=$true)][string]$BuildDirectory,
    [string]$VulkanSdk = ''
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..')).TrimEnd('\','/')
$build = [IO.Path]::GetFullPath($BuildDirectory).TrimEnd('\','/')
$buildRoot = [IO.Path]::GetFullPath((Join-Path $repo 'build'))
$insideBuild = $build.StartsWith($buildRoot + '\', [StringComparison]::OrdinalIgnoreCase)
$legacy = $build.StartsWith('W:\QoderCN\', [StringComparison]::OrdinalIgnoreCase)
if ($BuildDirectory -notmatch '^[Ww]:[\\/]' -or (-not $insideBuild -and -not $legacy)) { throw 'Use a new absolute build directory under W:/QoderCN or <repo>/build' }
if (Test-Path -LiteralPath $build) { throw 'Build directory must not exist; no overwrite or source reuse' }
if ($DependencyRoot -notmatch '^[A-Za-z]:[\\/]') { throw 'DependencyRoot must be absolute' }
function Hash([string]$Path) { (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant() }
function JsonFile($Value, [string]$Path) { [IO.File]::WriteAllText($Path, ($Value | ConvertTo-Json -Depth 30), [Text.UTF8Encoding]::new($false)) }
function Native([string]$Program, [string[]]$Arguments, [string]$Log) {
    $ErrorActionPreference = 'Continue'
    $lines = @(& $Program @Arguments 2>&1 | ForEach-Object { [string]$_ })
    $code = $LASTEXITCODE
    [IO.File]::WriteAllLines($Log, [string[]]$lines, [Text.UTF8Encoding]::new($false))
    JsonFile @{program=$Program; arguments=$Arguments; exit_code=$code} ($Log + '.command.json')
    if ($code -ne 0) { throw "Native command failed with exit code ${code}: $Program; see $Log" }
    return $lines
}
$sourceCommit = '4979e04f5dcaccb36057e059bbaed8a2f5288315'
$vulkan = $VulkanSdk -ne ''
$version = $(if ($vulkan) { '4979e04-stream-v2-vulkan' } else { '4979e04-stream-v1' })
$loaderDll = $null
$loaderLicense = $null
if ($vulkan) {
    if ($VulkanSdk -notmatch '^[A-Za-z]:[\\/]') { throw 'VulkanSdk must be absolute' }
    $VulkanSdk = [IO.Path]::GetFullPath($VulkanSdk).TrimEnd('\','/')
    $loaderDll = Join-Path $VulkanSdk 'Bin\vulkan-1.dll'
    $loaderLicense = Join-Path $VulkanSdk 'Share\Licenses\Vulkan-Loader-LICENSE.txt'
    foreach ($p in @((Join-Path $VulkanSdk 'Include\vulkan\vulkan.h'), (Join-Path $VulkanSdk 'Lib\vulkan-1.lib'), (Join-Path $VulkanSdk 'Bin\glslc.exe'), $loaderDll, $loaderLicense)) {
        if (-not (Test-Path -LiteralPath $p)) { throw "Vulkan SDK artifact missing: $p" }
    }
}
$archive = Join-Path $DependencyRoot 'whisper.tar.gz'
$json = Join-Path $DependencyRoot 'json.hpp'
$jsonLicense = Join-Path $DependencyRoot 'json-LICENSE.MIT'
if ((Hash $jsonLicense) -ne '46a65cffd1ea955132d95a8dd921640714a8d6b537d2e4e482d31145ae95b603') { throw 'JSON license SHA mismatch' }
if ((Hash $archive) -ne 'deb39eafbab81c95651c1c55a279fa0a635323c837dd23abe30358515cdbdcc7') { throw 'Whisper archive SHA mismatch' }
if ((Hash $json) -ne 'aaf127c04cb31c406e5b04a63f1ae89369fccde6d8fa7cdda1ed4f32dfc5de63') { throw 'JSON header SHA mismatch' }
New-Item -ItemType Directory -Path $build | Out-Null
trap {
    if (Test-Path -LiteralPath $build) { JsonFile @{schema=1; build='failed'; reason=$_.Exception.Message; clean_environment='not_verified'; inference='not_tested'} (Join-Path $build 'build-failure.json') }
    throw $_
}
$dependencyDir = Join-Path $build 'verified-source'
New-Item -ItemType Directory -Path $dependencyDir | Out-Null
$tar = "$env:SystemRoot\System32\tar.exe"
$null = Native $tar @('-xzf', $archive, '-C', $dependencyDir) (Join-Path $build 'extract.log')
$source = Join-Path $dependencyDir 'ggml-org-whisper.cpp-4979e04'
$vs = 'C:\Program Files (x86)\Microsoft Visual Studio\18\BuildTools'
Import-Module (Join-Path $vs 'Common7\Tools\Microsoft.VisualStudio.DevShell.dll')
Enter-VsDevShell -VsInstallPath $vs -SkipAutomaticLocation -DevCmdArguments '-arch=x64 -host_arch=x64'
$cmake = Join-Path $vs 'Common7\IDE\CommonExtensions\Microsoft\CMake\CMake\bin\cmake.exe'
$ninja = Join-Path $vs 'Common7\IDE\CommonExtensions\Microsoft\CMake\Ninja\ninja.exe'
$entry = [IO.Path]::GetFullPath($PSScriptRoot)
$obj = Join-Path $build 'obj'
$inputs = @()
$inputList = @($archive, $json, $jsonLicense, (Join-Path $source 'LICENSE'), (Join-Path $entry 'main.cpp'), (Join-Path $entry 'CMakeLists.txt'), (Join-Path $entry 'vad_test.cpp'), (Join-Path $entry 'utf8_test.cpp'), $PSCommandPath, $cmake, $ninja, $tar, (Get-Command cl.exe).Source, (Get-Command link.exe).Source, (Get-Command dumpbin.exe).Source)
if ($vulkan) { $inputList += @($loaderDll, $loaderLicense, (Join-Path $VulkanSdk 'Include\vulkan\vulkan.h'), (Join-Path $VulkanSdk 'Lib\vulkan-1.lib'), (Join-Path $VulkanSdk 'Bin\glslc.exe')) }
foreach ($p in $inputList) {
    $inputs += [ordered]@{name=$p; version=$sourceCommit; sha256=(Hash $p); license=$(if ($vulkan -and $p.StartsWith($VulkanSdk, [StringComparison]::OrdinalIgnoreCase)) { 'Apache-2.0' } elseif ($p.StartsWith($vs, [StringComparison]::OrdinalIgnoreCase) -or $p -eq $tar) { 'Microsoft-toolchain-build-only' } else { 'MIT/project-source' })}
}
$configureArgs = @('-S', $entry, '-B', $obj, '-G', 'Ninja', "-DCMAKE_MAKE_PROGRAM=$ninja", '-DCMAKE_BUILD_TYPE=Release', "-DWHISPER_SOURCE_DIR=$source", "-DJSON_HEADER=$json", '-DFETCHCONTENT_FULLY_DISCONNECTED=ON', '-DFETCHCONTENT_UPDATES_DISCONNECTED=ON', '-DGGML_BACKEND_DL=OFF', '-DGGML_RPC=OFF', '-DWHISPER_CURL=OFF')
if ($vulkan) {
    $env:VULKAN_SDK = $VulkanSdk
    $configureArgs += '-DWHISPER_STREAM_VULKAN=ON'
}
$null = Native $cmake $configureArgs (Join-Path $build 'configure.log')
$null = Native $cmake @('--build', $obj, '--target', 'whisper-stream', 'whisper-stream-vad-test', 'whisper-stream-utf8-test', '-j', '4') (Join-Path $build 'compile.log')
$ctest = Join-Path (Split-Path $cmake) 'ctest.exe'
$null = Native $ctest @('--test-dir', $obj, '--output-on-failure', '-V') (Join-Path $build 'regression.log')
JsonFile @{command=@($ctest, '--test-dir', $obj, '--output-on-failure', '-V'); exit_code=0; model_load=$false; tests=@('whisper-stream-vad','whisper-stream-utf8')} (Join-Path $build 'regression-evidence.json')
foreach ($input in $inputs) { if ((Hash $input.name) -cne $input.sha256) { throw "Build input changed: $($input.name)" } }
$delivery = Join-Path $build ('components\whisper-stream\' + $version)
New-Item -ItemType Directory -Path $delivery -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $obj 'whisper-stream.exe') -Destination $delivery
Copy-Item -LiteralPath (Join-Path $source 'LICENSE') -Destination (Join-Path $delivery 'whisper-LICENSE')
Copy-Item -LiteralPath $jsonLicense -Destination $delivery
if ($vulkan) {
    Copy-Item -LiteralPath $loaderDll -Destination $delivery
    Copy-Item -LiteralPath $loaderLicense -Destination $delivery
}
$exe = Join-Path $delivery 'whisper-stream.exe'
$imports = Native (Get-Command dumpbin.exe).Source @('/nologo', '/dependents', $exe) (Join-Path $delivery 'pe-dependencies.txt')
$dlls = @()
foreach ($line in $imports) {
    if ($line -match '^\s+([A-Za-z0-9_.-]+\.dll)\s*$') {
        $dll = $Matches[1]; $dlls += $dll
        if ($vulkan -and $dll -eq 'vulkan-1.dll') { continue }
        if ($dll -notmatch '^(KERNEL32|ADVAPI32|WS2_32|CRYPT32|BCRYPT|USER32|SHELL32|OLE32|SHLWAPI|MSVCRT|NTDLL|IPHLPAPI)\.dll$') { throw "Unclosed non-system DLL dependency: $dll" }
    }
}
if ($dlls.Count -eq 0) { throw 'No PE imports parsed; dependency audit inconclusive' }
if ($vulkan -and $dlls -notcontains 'vulkan-1.dll') { throw 'Vulkan build has no bundled loader dependency' }
# The native entry implements --metadata, not --component-info. Retain the real argv.
$metadata = Native $exe @('--metadata') (Join-Path $delivery 'metadata-smoke.json')
$info = ($metadata -join "`n") | ConvertFrom-Json
if ($info.component -cne 'whisper-stream' -or $info.protocol -cne 'fengshen.whispercpp.stream/v1' -or $info.native -isnot [bool] -or -not $info.native -or $info.source_commit -cne $sourceCommit -or $info.vad_supported -isnot [bool] -or -not $info.vad_supported -or ($vulkan -and ($info.PSObject.Properties.Name -notcontains 'gpu_backend' -or $info.gpu_backend -cne 'vulkan'))) { throw 'Native metadata identity/protocol/VAD/GPU mismatch' }
$evidence = [ordered]@{schema=1; component_id='whisper-stream'; binary_source='source-build'; source_commit=$sourceCommit; platform=$(if ($vulkan) { 'windows-amd64-vulkan' } else { 'windows-amd64-cpu' }); crt='static /MT'; gpu_backend=$(if ($vulkan) { 'vulkan' } else { 'none' }); system_dlls=$dlls; build_inputs=$inputs; metadata_arguments=@('--metadata'); vad_supported=$true; startup_test='passed'; clean_environment='not_verified'; inference='not_tested'}
if ($vulkan) {
    $evidence['bundled_runtime'] = @([ordered]@{path='vulkan-1.dll'; sha256=(Hash $loaderDll); sdk_version='1.4.363.0'; license='Apache-2.0'; fallback='cpu-max-threads'})
}
JsonFile $evidence (Join-Path $delivery 'build-evidence.json')
$files = @(Get-ChildItem -LiteralPath $delivery -File -Recurse | Sort-Object FullName | ForEach-Object { [ordered]@{path=$_.FullName.Substring($delivery.Length+1).Replace('\','/'); bytes=$_.Length.ToString([Globalization.CultureInfo]::InvariantCulture); sha256=(Hash $_.FullName)} })
JsonFile ([ordered]@{schema=1; component_id='whisper-stream'; version=$version; os='windows'; arch='amd64'; entry_type='executable'; entry='whisper-stream.exe'; packaging='native'; protocols=@('fengshen.whispercpp.stream/v1','--fengshen-stream-v1'); files=$files; runtime_dependencies=@(); model_refs=@('whisper-asr','whisper-vad'); driver_requirements=$(if ($vulkan) { @('Windows x64; Vulkan 1.2 driver optional, CPU fallback at max threads') } else { @('Windows x64 CPU') }); build_inputs=$inputs; startup_report_ref='startup-report.json'}) (Join-Path $delivery 'manifest.json')
JsonFile ([ordered]@{schema=1; component_id='whisper-stream'; version=$version; manifest_sha256=(Hash (Join-Path $delivery 'manifest.json')); startup_test='passed'; metadata_only=@{arguments=@('--metadata'); exit_code=0; checked_at=[DateTime]::UtcNow.ToString('o'); no_model_load=$true; no_network=$true; no_compilation=$true}; clean_environment='not_verified'; protocol_evidence='declared_not_tested'; inference='not_tested'}) (Join-Path $delivery 'startup-report.json')
# Report hashes are bound outside Manifest.Files, avoiding manifest/report cycles.
JsonFile @{schema=1; files=@(@{path='manifest.json'; sha256=(Hash (Join-Path $delivery 'manifest.json'))}, @{path='startup-report.json'; sha256=(Hash (Join-Path $delivery 'startup-report.json'))})} (Join-Path $build 'delivery-index.json')
Write-Output "Native source delivery: $delivery"
