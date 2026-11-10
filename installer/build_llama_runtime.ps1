param(
    [Parameter(Mandatory=$true)][string]$SourceArchive,
    [Parameter(Mandatory=$true)][string]$BuildDirectory,
    [ValidateSet('baseline','avx2')][string]$Profile = 'baseline'
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$entry = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../assets/llama/runtime'))
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..')).TrimEnd('\','/')
$build = [IO.Path]::GetFullPath($BuildDirectory).TrimEnd('\','/')
if (-not [IO.Path]::IsPathRooted($BuildDirectory) -or $build.StartsWith($repo, [StringComparison]::OrdinalIgnoreCase) -or -not $build.StartsWith('W:\QoderCN\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Use a new absolute build directory under W:/QoderCN outside the repository' }
if (Test-Path $build) { throw 'Build directory must not exist; no overwrite or mutable source reuse' }
$lock = Get-Content -Raw (Join-Path $entry 'source-lock.json') | ConvertFrom-Json
$selected = $lock.profiles.$Profile
$lock.version = $selected.version
$lock | Add-Member -NotePropertyName profile -NotePropertyValue $Profile
$lock | Add-Member -NotePropertyName driver_requirements -NotePropertyValue @($selected.driver_requirements)
function Hash([string]$p) { (Get-FileHash -LiteralPath $p -Algorithm SHA256).Hash.ToLowerInvariant() }
function JsonFile($value, [string]$p) { [IO.File]::WriteAllText($p, ($value | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding($false))) }
function Native([string]$Program, [string[]]$Arguments, [string]$Log, [int]$ExpectedExit = 0) {
    # Windows PowerShell 5 turns native stderr into ErrorRecord even on success.
    $ErrorActionPreference = 'Continue'
    $lines = @(& $Program @Arguments 2>&1 | ForEach-Object { "$_" })
    $code = $LASTEXITCODE
    [IO.File]::WriteAllLines($Log, [string[]]$lines)
    if ($code -ne $ExpectedExit) { throw "Native command failed ($code): $Program; see $Log" }
    return $lines
}
if ((Hash $SourceArchive) -ne $lock.archive_sha256) { throw 'Locked source archive SHA256 mismatch' }
New-Item -ItemType Directory -Path $build | Out-Null
$sourceRoot = Join-Path $build 'verified-source'
New-Item -ItemType Directory -Path $sourceRoot | Out-Null
& "$env:SystemRoot/System32/tar.exe" -xzf $SourceArchive -C $sourceRoot
if ($LASTEXITCODE -ne 0) { throw 'Source extraction failed' }
$source = Join-Path $sourceRoot $lock.archive_root
$vs = 'C:/Program Files (x86)/Microsoft Visual Studio/18/BuildTools'
Import-Module (Join-Path $vs 'Common7/Tools/Microsoft.VisualStudio.DevShell.dll')
Enter-VsDevShell -VsInstallPath $vs -SkipAutomaticLocation -DevCmdArguments '-arch=x64 -host_arch=x64'
$cmake = Join-Path $vs 'Common7/IDE/CommonExtensions/Microsoft/CMake/CMake/bin/cmake.exe'
$ninja = Join-Path $vs 'Common7/IDE/CommonExtensions/Microsoft/CMake/Ninja/ninja.exe'
$obj = Join-Path $build 'obj'
$null = Native $cmake @('-S', $entry, '-B', $obj, '-G', 'Ninja', "-DCMAKE_MAKE_PROGRAM=$ninja", '-DCMAKE_BUILD_TYPE=Release', "-DLLAMA_SOURCE_DIR=$source", "-DLLAMA_RUNTIME_PROFILE=$Profile") (Join-Path $build 'configure.log')
$null = Native $cmake @('--build', $obj, '--target', 'llama-runtime', 'llama-server', '-j', '4') (Join-Path $build 'compile.log')
$delivery = Join-Path $build ('components/llama-runtime/' + $lock.version)
$internal = Join-Path $delivery 'internal'
New-Item -ItemType Directory -Path $internal -Force | Out-Null
Copy-Item (Join-Path $obj 'llama-runtime.exe') $delivery
Copy-Item (Join-Path $obj 'bin/llama-server.exe') $internal
JsonFile $lock (Join-Path $delivery 'source-lock.json')
Copy-Item (Join-Path $entry 'supervisor-contract.json') $delivery
$notices = Join-Path $delivery 'licenses'
New-Item -ItemType Directory -Path $notices | Out-Null
Copy-Item (Join-Path $source 'LICENSE') (Join-Path $notices 'llama-LICENSE')
Copy-Item (Join-Path $source 'licenses') (Join-Path $notices 'upstream') -Recurse
# Preserve original bundled license texts, including notices embedded in header tails.
Copy-Item (Join-Path $source 'vendor') (Join-Path $notices 'vendor') -Recurse
$dependencyEvidence = @()
foreach ($exe in @((Join-Path $delivery 'llama-runtime.exe'), (Join-Path $internal 'llama-server.exe'))) {
    $imports = @(& dumpbin /nologo /dependents $exe)
    if ($LASTEXITCODE -ne 0) { throw 'dumpbin dependency audit failed' }
    $dependencyEvidence += $imports
    foreach ($line in $imports) {
        if ($line -match '^\s+([A-Za-z0-9_.-]+\.dll)\s*$') {
            $dll = $Matches[1]
            if ($dll -notmatch '^(KERNEL32|ADVAPI32|WS2_32|CRYPT32|BCRYPT|USER32|SHELL32|OLE32|SHLWAPI|MSVCRT|NTDLL|IPHLPAPI)\.dll$' -and $dll -notmatch '^api-ms-win-') { throw "Unclosed non-system DLL dependency: $dll" }
        }
    }
}
[IO.File]::WriteAllLines((Join-Path $delivery 'pe-dependencies.txt'), [string[]]$dependencyEvidence)
$exe = Join-Path $delivery 'llama-runtime.exe'
$metadata = @(& $exe --component-info)
$metadataExit = $LASTEXITCODE
$checkedAt = [DateTime]::UtcNow.ToString('o')
if ($metadataExit -ne 0) { throw 'Metadata-only smoke failed' }
$info = ($metadata -join "`n") | ConvertFrom-Json
if ($info.component_id -ne 'llama-runtime' -or $info.upstream_commit -ne $lock.commit -or $info.version -ne $lock.version -or $info.profile -ne $Profile -or ($info.driver_requirements -join "`n") -cne ($selected.driver_requirements -join "`n")) { throw 'Metadata identity mismatch' }
[IO.File]::WriteAllLines((Join-Path $delivery 'metadata-smoke.json'), [string[]]$metadata)
$null = Native $exe @('--model', 'forbidden') (Join-Path $build 'rejected-cli.log') 64
$server = Join-Path $internal 'llama-server.exe'
# AVX2 server execution is gated by the runtime adapter, not metadata smoke.
if ($Profile -eq 'baseline') {
    $versionOutput = Native $server @('--version') (Join-Path $delivery 'server-version.txt')
    if (($versionOutput -join ' ') -notmatch 'fd27b2034') { throw 'Built server version mismatch' }
    $null = Native $server @('--help') (Join-Path $delivery 'server-help.txt')
}
$compilerOutput = Native (Get-Command cl.exe).Source @('/?') (Join-Path $build 'compiler.txt')
$inputs = @([ordered]@{ name='llama.cpp-source'; version=$lock.commit; sha256=$lock.archive_sha256; license=$lock.license })
foreach ($p in @((Join-Path $entry 'component_info.cpp'), (Join-Path $entry 'component_profile.h.in'), (Join-Path $entry 'CMakeLists.txt'), (Join-Path $entry 'source-lock.json'), (Join-Path $entry 'supervisor-contract.json'), $PSCommandPath, $cmake, $ninja, (Get-Command cl.exe).Source, (Get-Command link.exe).Source)) {
    $inputs += [ordered]@{ name=[IO.Path]::GetFileName($p); version=$lock.version; sha256=(Hash $p); license=$(if ($p.StartsWith($vs, [StringComparison]::OrdinalIgnoreCase)) { 'Microsoft-toolchain-build-only' } else { 'project-source' }) }
}
$record = [ordered]@{ schema=1; binary_source='source-build'; upstream_binary_reused=$false; source_commit=$lock.commit; platform=$lock.platform; crt='static /MT'; version=$lock.version; profile=$Profile; driver_requirements=@($selected.driver_requirements); backend=$selected.backend; build_inputs=$inputs; compiler=($compilerOutput -join "`n"); model_load='not_tested'; text_inference='not_tested'; vision_inference='not_tested'; authenticated_startup='not_tested'; clean_environment='not_verified' }
JsonFile $record (Join-Path $delivery 'build-evidence.json')
$files = @(Get-ChildItem $delivery -File -Recurse | Sort-Object FullName | ForEach-Object { [ordered]@{ path=$_.FullName.Substring($delivery.Length+1).Replace('\','/'); bytes=$_.Length.ToString([Globalization.CultureInfo]::InvariantCulture); sha256=(Hash $_.FullName) } })
$manifest = [ordered]@{ schema=1; component_id='llama-runtime'; version=$lock.version; os='windows'; arch='amd64'; entry_type='executable'; entry='llama-runtime.exe'; packaging='native'; protocols=@('llama.cpp.server.http/b12182'); files=$files; runtime_dependencies=@(); model_refs=@('local-text','local-vision'); driver_requirements=@($selected.driver_requirements); build_inputs=$inputs; startup_report_ref='startup-report.json' }
JsonFile $manifest (Join-Path $delivery 'manifest.json')
$report = [ordered]@{ schema=1; component_id='llama-runtime'; version=$lock.version; manifest_sha256=(Hash (Join-Path $delivery 'manifest.json')); startup_test='passed'; metadata_only=[ordered]@{ arguments=@('--component-info'); exit_code=$metadataExit; checked_at=$checkedAt; no_model_load=$true; no_network=$true; no_compilation=$true }; clean_environment='not_verified'; protocol_evidence='declared_not_tested'; inference='not_tested' }
JsonFile $report (Join-Path $delivery 'startup-report.json')
# Bind exact manifest/report bytes outside Manifest.Files to avoid a hash cycle.
JsonFile ([ordered]@{ schema=1; files=@(@{ path='manifest.json'; sha256=(Hash (Join-Path $delivery 'manifest.json')) }, @{ path='startup-report.json'; sha256=(Hash (Join-Path $delivery 'startup-report.json')) }) }) (Join-Path $build 'delivery-index.json')
Write-Output "Native source delivery: $delivery"
