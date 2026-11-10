[CmdletBinding()]
param(
    [string]$Output = 'W:\QoderCN\model-center-build',
    [string]$Version = 'dev-local',
    # Optional offline, fully locked input tree. No mutable sidecar/venv is used.
    [string]$Inputs = '',
    [string]$InputLock = '',
    [switch]$FreezeDemucs,
    [switch]$FreezeSeedVC,
    [switch]$BuildWhisperBatch,
    [string]$BuildWhisperBatchDependencyRoot = '',
    [switch]$BuildWhisperStream,
    [string]$WhisperStreamDependencyRoot = '',
    [switch]$BuildLlamaRuntime,
    [string]$LlamaSourceArchive = '',
    [ValidateSet('baseline','avx2')][string]$LlamaProfile = 'baseline',
    # Local backend EXEs only; no UI packaging, signing, publishing or version changes.
    [switch]$BuildEngines
)
$ErrorActionPreference = 'Stop'
# Native Go output is UTF-8; keep retained diagnostics readable in Windows PS 5.
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$module = $repo
$Output = [IO.Path]::GetFullPath($Output)
function RelativePath([string]$Base, [string]$Target) {
    $baseUri = [Uri]::new($Base.TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar)
    $targetUri = [Uri]::new($Target)
    return [Uri]::UnescapeDataString($baseUri.MakeRelativeUri($targetUri).ToString()).Replace('/', [IO.Path]::DirectorySeparatorChar)
}
$relative = RelativePath $repo $Output
if (-not ($relative -eq '..' -or $relative.StartsWith('..' + [IO.Path]::DirectorySeparatorChar))) {
    throw 'Build output must be outside the repository.'
}
if ($Version -notmatch '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$') { throw 'Invalid component version.' }
if ([bool]$Inputs -ne [bool]$InputLock) { throw 'Inputs and InputLock must be supplied together.' }
if (Test-Path $Output) { throw 'Use a fresh output directory; old evidence must not survive a rebuild.' }
function Run([string]$Program, [string[]]$Arguments) {
    # Preserve actual gate diagnostics outside the repository, including stderr.
    $ErrorActionPreference = 'Continue'
    & $Program @Arguments 2>&1 | ForEach-Object {
        [IO.File]::AppendAllText((Join-Path $Output 'build-transcript.log'), ([string]$_ + [Environment]::NewLine), [Text.UTF8Encoding]::new($false))
        Write-Output ([string]$_)
    }
    if ($LASTEXITCODE -ne 0) { throw "$Program $($Arguments -join ' ') failed with exit code $LASTEXITCODE; see build-transcript.log" }
}
function JsonFile([string]$Path, $Value) {
    $json = ConvertTo-Json -InputObject $Value -Depth 30
    [IO.File]::WriteAllText($Path, $json, [Text.UTF8Encoding]::new($false))
}
New-Item -ItemType Directory -Path $Output | Out-Null
trap {
    if (Test-Path $Output) {
        JsonFile (Join-Path $Output 'build-failure.json') @{schema=1; build='failed'; reason=$_.Exception.Message; startup_test='not_verified'; clean_environment='not_verified'; inference='not_tested'}
        # Do not mutate evidence already bound by delivery.json. Pipeline failure
        # and partial engine results are separate from completed component smoke.
        if (-not (Test-Path (Join-Path $Output 'build-results.json'))) {
            $failedResults = @('model-center','whisper-batch','whisper-stream','demucs-runner','seedvc-runner','haar-consumer','llama-runtime') | ForEach-Object { @{component_id=$_; build='not_verified'; startup_test='not_tested'; clean_environment='not_verified'; inference='not_tested'; reason='Build pipeline stopped before component delivery.'} }
            JsonFile (Join-Path $Output 'build-results.json') @($failedResults)
        }
    }
    throw $_
}
$go = (Get-Command go -CommandType Application | Select-Object -First 1).Source
# Prevent Go from auto-fetching another toolchain or missing modules.
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
$env:GOSUMDB = 'off'
$env:GOWORK = 'off'
$env:CGO_ENABLED = '0'
$goVersion = (& $go version | Out-String).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Go toolchain unavailable.' }
$env:GOFLAGS = '-mod=readonly'
$env:PATH = (Split-Path $go) + [IO.Path]::PathSeparator + $env:PATH
# Resolve Go's complete dependency/embed/test graph, local replaces and compiler
# inputs; also pin this script and the frozen entry. No mutable venv is trusted.
$snapshotArgs = @('-C', $module, 'run', './build', '-mode', 'snapshot', '-root', $repo, '-output', $Output)
if ($BuildEngines) { $snapshotArgs += '-engines' }
Run $go $snapshotArgs
$inputLockSHA = if ($InputLock) { (Get-FileHash -LiteralPath $InputLock -Algorithm SHA256).Hash } else { '' }
function Assert-Inputs {
    # Resolve the input set again as well as checking each identity/hash: newly
    # added Go/embed/module inputs must not silently escape the original lock.
    $verifyArgs = @('-C', $module, 'run', './build', '-mode', 'verify-snapshot', '-root', $repo, '-output', $Output)
    if ($BuildEngines) { $verifyArgs += '-engines' }
    Run $go $verifyArgs
    if ($Inputs -and $InputLock) {
        if ((Get-FileHash -LiteralPath $InputLock -Algorithm SHA256).Hash -cne $inputLockSHA) { throw 'External input lock changed.' }
        Run $go @('-C', $module, 'run', './build', '-mode', 'verify-lock', '-root', $Inputs, '-template', $InputLock)
        # verify-lock hashes every listed file; also reject any unlisted file on EVERY check.
        $currentLock = Get-Content -LiteralPath $InputLock -Raw | ConvertFrom-Json
        $lockedPaths = @($currentLock.inputs | ForEach-Object { $_.path.ToLowerInvariant() })
        foreach ($file in @(Get-ChildItem -LiteralPath $Inputs -Recurse -Force -File)) {
            $rel = (RelativePath ([IO.Path]::GetFullPath($Inputs)) $file.FullName).Replace('\','/').ToLowerInvariant()
            if ($rel -notin $lockedPaths) { throw "Unpinned offline input: $rel" }
        }
    }
}
JsonFile (Join-Path $Output 'toolchain.json') @{go_version=$goVersion; go_executable_sha256=(Get-FileHash $go -Algorithm SHA256).Hash.ToLower(); inference='not_tested'}
Assert-Inputs
Run $go @('-C', $module, 'test', './...')
Run $go @('-C', $module, 'vet', './...')
Assert-Inputs
$results = @()
function Manifest([string]$Id, [string]$Directory, [string]$Entry, [string]$Packaging, $BuildInputs, [string[]]$ModelRefs = @(), [string]$StartupReportRef = 'startup-report.json', [string[]]$Protocols = @()) {
    $template = Join-Path $Output ($Id + '.template.json')
    JsonFile $template @{schema=1; component_id=$Id; version=$Version; os='windows'; arch='amd64'; entry_type='executable'; entry=$Entry; packaging=$Packaging; protocols=$Protocols; files=@(); runtime_dependencies=@(); model_refs=$ModelRefs; driver_requirements=@(); build_inputs=$BuildInputs; startup_report_ref=$StartupReportRef}
    Run $go @('-C', $module, 'run', './build', '-mode', 'manifest', '-root', $Directory, '-template', $template)
}
$goTarget = (& $go env GOOS GOARCH)
if (($goTarget -join '/') -ne 'windows/amd64') { throw 'This script currently verifies only windows/amd64 builds.' }
function Metadata-Smoke([string]$Id, [string]$Directory, [string]$Entry, [string]$Packaging, [string]$MetadataArgument = '--component-info', [string[]]$Protocols = @(), $BuildInputs = @(), [string[]]$ModelRefs = @()) {
    # Explicit metadata-only execution in the build host, NOT an isolated VM test.
    if ($MetadataArgument -cne '--component-info' -and -not ($Id -ceq 'haar-consumer' -and $MetadataArgument -ceq '--haar-component-info')) { throw 'Unsupported component metadata argument.' }
    $start = [Diagnostics.ProcessStartInfo]::new((Join-Path $Directory $Entry), $MetadataArgument)
    $start.UseShellExecute=$false; $start.RedirectStandardOutput=$true; $start.RedirectStandardError=$true; $start.WorkingDirectory=$Directory
    $p = [Diagnostics.Process]::Start($start)
    $stdoutTask=$p.StandardOutput.ReadToEndAsync(); $stderrTask=$p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit(15000)) { $p.Kill(); $p.Dispose(); throw 'component-info timed out' }
    $text=$stdoutTask.GetAwaiter().GetResult(); $stderr=$stderrTask.GetAwaiter().GetResult(); $exitCode=$p.ExitCode; $p.Dispose()
    if ($exitCode -ne 0) { throw "component-info failed: $stderr" }
    $info=$text | ConvertFrom-Json
    if ($info.schema -ne 1 -or $info.component_id -cne $Id -or $info.inference -cne 'not_tested' -or $info.packaging -cne $Packaging) { throw 'component-info identity mismatch' }
    if ($Id -in @('model-center','haar-consumer') -and ($info.version -cne $Version -or $info.os -cne 'windows' -or $info.arch -cne 'amd64')) { throw 'Center component-info version/platform mismatch' }
    if ($Protocols.Count) {
        $declaredProtocols = @($info.protocols)
        if ($declaredProtocols.Count -ne $Protocols.Count) { throw 'component-info protocol mismatch' }
        for ($i = 0; $i -lt $Protocols.Count; $i++) {
            if ($declaredProtocols[$i] -cne $Protocols[$i]) { throw 'component-info protocol mismatch' }
        }
    }
    if ($Id -ceq 'haar-consumer' -and ($MetadataArgument -cne '--haar-component-info' -or $info.embedded -isnot [bool] -or -not $info.embedded -or $Protocols.Count -ne 1 -or $Protocols[0] -cne 'haar/v1')) { throw 'Haar must declare the embedded metadata entry and haar/v1.' }
    $protocolEvidence = if ($Protocols.Count) { 'declared_not_tested' } else { 'not_declared' }
    JsonFile (Join-Path $Directory 'component-info.json') $info
    Manifest $Id $Directory $Entry $Packaging $BuildInputs $ModelRefs 'startup-report.json' $Protocols
    JsonFile (Join-Path $Directory 'startup-report.json') @{schema=1; component_id=$Id; version=$Version; manifest_sha256=(Get-FileHash (Join-Path $Directory 'manifest.json') -Algorithm SHA256).Hash.ToLower(); startup_test='passed'; metadata_only=@{arguments=@($MetadataArgument); exit_code=$exitCode; checked_at=[DateTime]::UtcNow.ToString('o'); no_model_load=$true; no_network=$true; no_compilation=$true}; clean_environment='not_verified'; protocol_evidence=$protocolEvidence; inference='not_tested'}
}
$centerDir = Join-Path $Output ('model-center\components\model-center\' + $Version)
New-Item -ItemType Directory -Path $centerDir -Force | Out-Null
Assert-Inputs
Run $go @('-C', $module, 'build', '-trimpath', '-buildvcs=false', '-ldflags', "-X main.buildVersion=$Version", '-o', (Join-Path $centerDir 'fengshen-model-center.exe'), '.')
$centerBuildInputs = @(@{name='Go'; version=$goVersion; sha256=(Get-FileHash $go -Algorithm SHA256).Hash.ToLower(); license='BSD-3-Clause'})
Metadata-Smoke 'model-center' $centerDir 'fengshen-model-center.exe' 'go' -BuildInputs $centerBuildInputs
Copy-Item (Join-Path $centerDir 'fengshen-model-center.exe') (Join-Path $Output 'model-center\fengshen-model-center.exe')
$results += @{component_id='model-center'; build='built'; startup_test='passed'; clean_environment='not_verified'; inference='not_tested'; reason='Version/platform-checked --component-info only; host isolation and inference were not tested.'}
$lockData = $null
if ($Inputs -and $InputLock) {
    $Inputs = [IO.Path]::GetFullPath($Inputs)
    Run $go @('-C', $module, 'run', './build', '-mode', 'verify-lock', '-root', $Inputs, '-template', $InputLock)
    $lockData = Get-Content -LiteralPath $InputLock -Raw | ConvertFrom-Json
    # Ensure no unpinned input is supplied to a freezer, including Python DLLs.
    $actual = @(Get-ChildItem $Inputs -Recurse -File)
    $locked = @($lockData.inputs | ForEach-Object {$_.path.ToLower()})
    foreach ($file in $actual) {
        $rel = (RelativePath $Inputs $file.FullName).Replace('\','/').ToLower()
        if ($rel -notin $locked) { throw "Unpinned offline input: $rel" }
    }
    Copy-Item $InputLock (Join-Path $Output 'external-inputs.lock.json')
}
foreach ($id in @('demucs-runner','seedvc-runner')) {
    $requested = ($id -eq 'demucs-runner' -and $FreezeDemucs) -or ($id -eq 'seedvc-runner' -and $FreezeSeedVC)
    if (-not $requested -or -not $lockData) {
        $results += @{component_id=$id; build='pending_build'; startup_test='not_tested'; clean_environment='not_verified'; inference='not_tested'; reason='No explicitly requested, complete offline tool/dependency/license lock.'}
        continue
    }
    try {
        $python = Join-Path $Inputs 'python\python.exe'
        if (-not (Test-Path $python)) { throw 'Missing locked private build Python.' }
        $stage = Join-Path $Output ($id + '-stage')
        New-Item -ItemType Directory -Path $stage | Out-Null
        Copy-Item (Join-Path $repo 'sidecar\model_center_frozen_runner.py') (Join-Path $stage 'model_center_frozen_runner.py')
        $args = @('-I','-m','PyInstaller','--noconfirm','--clean','--onedir','--name',$id,'--distpath',(Join-Path $Output 'freeze-dist'),'--workpath',(Join-Path $stage 'work'),'--specpath',$stage)
        if ($id -eq 'demucs-runner') {
            Assert-Inputs
            # Resolve only the Demucs package used by the locked private Python.
            # Both official YAMLs must themselves be listed and hash-pinned inputs.
            $demucsBags = @'
import hashlib, importlib.util, json, pathlib, shutil, sys
import yaml
inputs, lock, target = map(pathlib.Path, sys.argv[1:4])
mode = sys.argv[4]
inputs = inputs.resolve()
pins = json.loads(lock.read_text(encoding='utf-8-sig'))['inputs']
package = pathlib.Path(importlib.util.find_spec('demucs').origin).resolve().parent
for name, signatures, weights in (
    ('htdemucs', ['955717e8'], None),
    ('htdemucs_ft', ['f7e0c4bc', 'd12395a8', '92cfc3b6', '04573f0d'],
     [[float(i == j) for j in range(4)] for i in range(4)])):
    source = package / 'remote' / (name + '.yaml')
    relative = source.resolve().relative_to(inputs).as_posix()
    matches = [pin for pin in pins if pin['path'] == relative]
    if len(matches) != 1 or source.is_symlink():
        raise RuntimeError('Missing uniquely locked official Demucs bag: ' + relative)
    content = source.read_bytes()
    if hashlib.sha256(content).hexdigest() != matches[0]['sha256'].lower():
        raise RuntimeError('Locked Demucs bag hash mismatch: ' + relative)
    bag = yaml.safe_load(content)
    if (not isinstance(bag, dict) or set(bag) - {'models', 'weights', 'segment'} or
            bag.get('models') != signatures or bag.get('weights') != weights):
        raise RuntimeError('Unsupported official Demucs bag: ' + name)
    destination = target / 'resources' / 'demucs' / (name + '.yaml')
    if mode == 'stage':
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination)
    if not destination.is_file() or destination.is_symlink() or destination.read_bytes() != content:
        raise RuntimeError('Missing or changed staged Demucs bag: ' + name)
'@
            Run $python @('-I','-B','-c',$demucsBags,$Inputs,$InputLock,$stage,'stage')
            $args += @('--collect-all','demucs','--collect-all','torch','--collect-all','torchaudio')
        } else {
            # Prepared inference module/config tree must itself be fully pinned.
            $seed = Join-Path $Inputs 'seedvc'
            if (-not (Test-Path (Join-Path $seed 'seedvc_inference.py'))) { throw 'Missing locked Seed-VC module alias and resource closure.' }
            $args += @('--paths',$seed,'--hidden-import','seedvc_inference','--collect-all','torch','--collect-all','torchaudio','--exclude-module','dac','--exclude-module','audiotools','--exclude-module','matplotlib','--add-data',($seed + ';seedvc'))
        }
        $args += (Join-Path $stage 'model_center_frozen_runner.py')
        Run $python $args
        $dir = Join-Path $Output ('model-center\components\' + $id + '\' + $Version)
        New-Item -ItemType Directory -Path (Split-Path $dir) -Force | Out-Null
        Move-Item (Join-Path $Output ('freeze-dist\' + $id)) $dir
        if ($id -eq 'demucs-runner') {
            Assert-Inputs
            Copy-Item -LiteralPath (Join-Path $stage 'resources') -Destination $dir -Recurse
            # Assert the fixed external resource closure before manifest hashing.
            Run $python @('-I','-B','-c',$demucsBags,$Inputs,$InputLock,$dir,'verify')
        }
        $pins = @($lockData.inputs | ForEach-Object {@{name=$_.path; version=$_.version; sha256=$_.sha256; license=$_.license}})
        $protocol = if ($id -eq 'demucs-runner') { 'fengshen.demucs.cli/v1' } else { 'fengshen.seedvc.cli/v1' }
        $modelRefs = if ($id -eq 'demucs-runner') { @('demucs','demucs-ft') } else { @('seed-vc','seed-whisper','campplus','bigvgan') }
        Metadata-Smoke $id $dir ($id + '.exe') 'pyinstaller-onedir' -Protocols @($protocol) -BuildInputs $pins -ModelRefs $modelRefs
        $results += @{component_id=$id; build='built'; startup_test='passed'; clean_environment='not_verified'; inference='not_tested'}
    } catch {
        $results += @{component_id=$id; build='failed'; startup_test='not_verified'; inference='not_tested'; reason=$_.Exception.Message}
    }
}
# Removed the private stock whisper-cli build: it cannot implement the runtime contract.
# Each native builder receives its own new bounded directory and runs sequentially.
# Do not ingest previously built component trees or guess a target by wildcard.
foreach ($id in @('whisper-batch','whisper-stream','llama-runtime')) {
    $requested = if ($id -eq 'whisper-batch') { [bool]$BuildWhisperBatch } elseif ($id -eq 'whisper-stream') { [bool]$BuildWhisperStream } else { [bool]$BuildLlamaRuntime }
    $inputPath = if ($id -eq 'whisper-batch') { $BuildWhisperBatchDependencyRoot } elseif ($id -eq 'whisper-stream') { $WhisperStreamDependencyRoot } else { $LlamaSourceArchive }
    if (-not $requested -or -not $inputPath) {
        $results += @{component_id=$id; build='pending_build'; startup_test='not_tested'; clean_environment='not_verified'; inference='not_tested'; reason='Native implementation exists; explicit build switch and offline input path are required. No old binary is trusted.'}
        continue
    }
    $nativeBuild = Join-Path $Output ($id + '-native-build')
    if ($inputPath -notmatch '^[A-Za-z]:[\\/]' -or -not (Test-Path -LiteralPath $inputPath)) { throw 'Native offline input must be an existing absolute path.' }
    if (-not $Output.StartsWith('W:\QoderCN\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Native builds require a new output under W:/QoderCN.' }
    Assert-Inputs
    $shell = Join-Path $PSHOME 'powershell.exe'
    if ($id -eq 'whisper-batch') {
        Run $shell @('-NoProfile','-File',(Join-Path $PSScriptRoot 'build_whisper_batch.ps1'),'-DependencyRoot',$inputPath,'-BuildDirectory',$nativeBuild)
        $nativeVersion = '4979e04-batch-v2'
    } elseif ($id -eq 'whisper-stream') {
        Run $shell @('-NoProfile','-ExecutionPolicy','Bypass','-File',(Join-Path $PSScriptRoot 'build_whisper_stream.ps1'),'-DependencyRoot',$inputPath,'-BuildDirectory',$nativeBuild)
        $nativeVersion = '4979e04-stream-v1'
    } else {
        Run $shell @('-NoProfile','-ExecutionPolicy','Bypass','-File',(Join-Path $PSScriptRoot 'build_llama_runtime.ps1'),'-SourceArchive',$inputPath,'-BuildDirectory',$nativeBuild,'-Profile',$LlamaProfile)
        $nativeLock = Get-Content -LiteralPath (Join-Path $repo 'components-src\llama-runtime\source-lock.json') -Raw | ConvertFrom-Json
        $nativeVersion = $nativeLock.profiles.$LlamaProfile.version
    }
    Assert-Inputs
    $from = Join-Path $nativeBuild ('components\' + $id + '\' + $nativeVersion)
    $nativeManifest = Get-Content -LiteralPath (Join-Path $from 'manifest.json') -Raw | ConvertFrom-Json
    if ($nativeManifest.component_id -cne $id -or $nativeManifest.version -cne $nativeVersion) { throw 'Native output identity mismatch.' }
    $destination = Join-Path $Output ('model-center\components\' + $id + '\' + $nativeVersion)
    New-Item -ItemType Directory -Path (Split-Path $destination) -Force | Out-Null
    Move-Item -LiteralPath $from -Destination $destination
    $results += @{component_id=$id; build='built'; startup_test='passed'; clean_environment='not_verified'; inference='not_tested'; reason='Fresh source build and metadata-only entry; protocol declared, inference not tested.'}
}
# Haar executes inside the real center Go binary, not a separate algorithm copy.
# Its component entry is the same freshly built executable, never a stub or old EXE.
Assert-Inputs
$haarDir = Join-Path $Output ('model-center\components\haar-consumer\' + $Version)
New-Item -ItemType Directory -Path $haarDir -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $centerDir 'fengshen-model-center.exe') -Destination (Join-Path $haarDir 'fengshen-model-center.exe')
if ((Get-FileHash (Join-Path $haarDir 'fengshen-model-center.exe') -Algorithm SHA256).Hash -cne (Get-FileHash (Join-Path $centerDir 'fengshen-model-center.exe') -Algorithm SHA256).Hash) { throw 'Embedded Haar copy differs from the built center.' }
Metadata-Smoke 'haar-consumer' $haarDir 'fengshen-model-center.exe' 'go' '--haar-component-info' @('haar/v1') -BuildInputs $centerBuildInputs -ModelRefs @('haar-frontal','haar-profile')
$results += @{component_id='haar-consumer'; build='built'; startup_test='passed'; clean_environment='not_verified'; inference='not_tested'; reason='Shared facehaar is embedded in the same-byte center copy; --haar-component-info checked version/platform/go and declared haar/v1 only. No separate algorithm, clean-environment or inference success claimed.'}

# Revalidate sources after compilation; refuse evidence for a moving source tree.
Assert-Inputs
JsonFile (Join-Path $Output 'build-results.json') $results
JsonFile (Join-Path $Output 'resource-layout.json') @{schema=1; center='model-center/fengshen-model-center.exe'; components='model-center/components'; root_kind='installed'; model_roots=@(); haar_files=@()}
Run $go @('-C', $module, 'run', './build', '-mode', 'delivery', '-root', $Output)
$exe = Join-Path $Output 'model-center\fengshen-model-center.exe'
$manifest = Join-Path $centerDir 'manifest.json'
$centerSHA = (Get-FileHash $exe -Algorithm SHA256).Hash.ToLower()
$deliverySHA = (Get-FileHash (Join-Path $Output 'model-center\delivery.json') -Algorithm SHA256).Hash.ToLower()
$injection = "-X github.com/aura-bootstrap/fengshen_model_center/integration.CenterSHA256=$centerSHA -X github.com/aura-bootstrap/fengshen_model_center/integration.DeliverySHA256=$deliverySHA"
JsonFile (Join-Path $Output 'engine-build-inputs.json') @{schema=1; executable_relative='model-center/fengshen-model-center.exe'; executable_sha256=$centerSHA; manifest_sha256=(Get-FileHash $manifest -Algorithm SHA256).Hash.ToLower(); delivery_sha256=$deliverySHA; ldflags=$injection; dev_ldflags=($injection + ' -X fengshen-slicer/internal/build.Flavor=internal'); inference='not_tested'}
if ($BuildEngines) {
    $engineResults = @()
    foreach ($edition in @('admin','dev','user')) {
        Assert-Inputs
        $backend = Join-Path $repo ('src\' + $edition + '_client\backend')
        $engineRoot = Join-Path $Output ('engines\' + $edition)
        New-Item -ItemType Directory -Path $engineRoot -Force | Out-Null
        # Mirror exactly the files bound by delivery, plus its digest-pinned file.
        # Component binaries are also required by the read-only component manager.
        Copy-Item (Join-Path $Output 'model-center') (Join-Path $engineRoot 'model-center') -Recurse
        $delivery = Get-Content (Join-Path $Output 'model-center\delivery.json') -Raw | ConvertFrom-Json
        foreach ($file in $delivery.files) {
            if ($file.path.StartsWith('model-center/')) { continue }
            $target = Join-Path $engineRoot $file.path
            New-Item -ItemType Directory -Path (Split-Path $target) -Force | Out-Null
            Copy-Item (Join-Path $Output $file.path) $target
        }
        $flags = $injection
        if ($edition -eq 'dev') { $flags += ' -X fengshen-slicer/internal/build.Flavor=internal' }
        Run $go @('-C', $backend, 'test', '-count=1', './cmd/fengshen-engine')
        Run $go @('-C', $backend, 'vet', './cmd/fengshen-engine')
        Run $go @('-C', $backend, 'build', '-trimpath', '-buildvcs=false', '-ldflags', $flags, '-o', (Join-Path $engineRoot 'fengshen-engine.exe'), './cmd/fengshen-engine')
        Assert-Inputs
        Run $go @('-C', $module, 'run', './build', '-mode', 'delivery', '-root', $engineRoot)
        $copiedSHA = (Get-FileHash (Join-Path $engineRoot 'model-center\delivery.json') -Algorithm SHA256).Hash.ToLower()
        if ($copiedSHA -cne $deliverySHA) { throw 'Copied engine resource delivery digest mismatch.' }
        $engineResults += @{edition=$edition; build='built'; test='passed'; vet='passed'; executable_sha256=(Get-FileHash (Join-Path $engineRoot 'fengshen-engine.exe') -Algorithm SHA256).Hash.ToLower(); center_sha256=$centerSHA; delivery_sha256=$deliverySHA; ldflags=$flags; startup_test='not_tested'; inference='not_tested'}
        JsonFile (Join-Path $Output 'engine-build-results.json') $engineResults
    }
    # Trust is post-build only: never include it in delivery or feed it into ldflags.
    # Each install root contains only its own final engine.
    $trustResults = @()
    foreach ($edition in @('admin','dev','user')) {
        Assert-Inputs
        $engineRoot = Join-Path $Output ('engines\' + $edition)
        $trustEdition = if ($edition -eq 'dev') { 'developer' } else { $edition }
        $trustTemplate = Join-Path $Output ($edition + '-engine-trust.template.json')
        JsonFile $trustTemplate @{schema=1; engines=@(@{inputpath='fengshen-engine.exe'; edition=$trustEdition; management=($edition -ne 'user')})}
        Run $go @('-C', $module, 'run', './build', '-mode', 'engine-trust', '-root', $engineRoot, '-template', $trustTemplate, '-center-sha256', $centerSHA, '-delivery-sha256', $deliverySHA)
        $trustResults += @{edition=$edition; build='built'; path='model-center/engine-trust.json'; sha256=(Get-FileHash (Join-Path $engineRoot 'model-center\engine-trust.json') -Algorithm SHA256).Hash.ToLower(); center_sha256=$centerSHA; delivery_sha256=$deliverySHA; inference='not_tested'}
        JsonFile (Join-Path $Output 'engine-trust-build-results.json') $trustResults
    }
}
Assert-Inputs
Write-Output "Local artifacts: $Output. No clean-environment or inference verification claimed."
