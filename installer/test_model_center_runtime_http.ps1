[CmdletBinding()]
param(
    [switch]$Run,
    [string]$BuildParent = [IO.Path]::GetTempPath(),
    [string]$FaceJpeg = '',
    [string]$ProfileJpeg = '',
    [switch]$BuildWhisperBatch,
    [string]$WhisperBatchDependencyRoot = '',
    [switch]$BuildWhisperStream,
    [string]$WhisperStreamDependencyRoot = '',
    [string]$ASR = '',
    [string]$VAD = '',
    [string]$SpeechWav = ''
)
$ErrorActionPreference = 'Stop'
# No build, scan, model load, download or center startup without explicit consent.
if (-not $Run) {
    Write-Output 'NOT RUN: real independent-center HTTP inference requires -Run -FaceJpeg <absolute real messi5.jpg>.'
    Write-Output 'Optional profile: -ProfileJpeg <absolute genuine profile JPEG with reviewed source/timestamp/SHA; no synthetic images>. Original and horizontal reflection must both classify as profile; omission is NOT RUN.'
    Write-Output 'Optional batch: -BuildWhisperBatch -WhisperBatchDependencyRoot <absolute offline dependencies> -ASR <absolute real GGML> -SpeechWav <absolute real speech WAV>.'
    Write-Output 'Optional stream: -BuildWhisperStream -WhisperStreamDependencyRoot <absolute offline dependencies> -ASR <absolute real GGML> -VAD <absolute real Silero GGML> -SpeechWav <absolute real speech WAV>.'
    Write-Output 'Native batch/stream builds require -BuildParent <existing directory under W:/QoderCN>.'
    return
}
if ($env:OS -ne 'Windows_NT') { throw 'Windows is required.' }
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$module = $repo
function ExistingAbsolute([string]$Path, [bool]$Directory) {
    if (-not $Path -or -not [IO.Path]::IsPathRooted($Path)) { throw 'Explicit absolute existing input required; no helper/download fallback.' }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer -ne $Directory -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Input must be an ordinary file/directory.' }
    if (-not $Directory -and $item.Length -le 0) { throw 'Empty real input refused.' }
    return $item.FullName
}
$BuildParent = ExistingAbsolute $BuildParent $true
$FaceJpeg = ExistingAbsolute $FaceJpeg $false
if ([IO.Path]::GetFileName($FaceJpeg) -ine 'messi5.jpg') { throw 'Select the real messi5.jpg explicitly.' }
if ($ProfileJpeg) { $ProfileJpeg = ExistingAbsolute $ProfileJpeg $false }
if (-not $BuildWhisperBatch -and $WhisperBatchDependencyRoot) { throw 'Batch dependencies require explicit -BuildWhisperBatch.' }
if (-not $BuildWhisperStream -and ($WhisperStreamDependencyRoot -or $VAD)) { throw 'Stream dependencies and VAD require explicit -BuildWhisperStream.' }
if (-not ($BuildWhisperBatch -or $BuildWhisperStream) -and ($ASR -or $SpeechWav)) { throw 'ASR and speech inputs require explicit -BuildWhisperBatch or -BuildWhisperStream.' }
if ($BuildWhisperBatch -or $BuildWhisperStream) {
    if (-not ($BuildParent.TrimEnd('\','/') + '\').StartsWith('W:\QoderCN\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Native builds require -BuildParent under W:/QoderCN.' }
    $ASR = ExistingAbsolute $ASR $false
    $SpeechWav = ExistingAbsolute $SpeechWav $false
}
if ($BuildWhisperBatch) {
    $WhisperBatchDependencyRoot = ExistingAbsolute $WhisperBatchDependencyRoot $true
}
if ($BuildWhisperStream) {
    $WhisperStreamDependencyRoot = ExistingAbsolute $WhisperStreamDependencyRoot $true
    $VAD = ExistingAbsolute $VAD $false
}
$out = Join-Path $BuildParent ('fengshen-runtime-http-' + [Guid]::NewGuid().ToString('N'))
if ($out.StartsWith($repo.TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Build root must be outside the repository.' }
if (Test-Path -LiteralPath $out) { throw 'Fresh unique build root required.' }
$names = @('PATH','GOTOOLCHAIN','GOPROXY','GOSUMDB','GOWORK','CGO_ENABLED','GOFLAGS',
    'FENGSHEN_CENTER_RUNTIME_HTTP_E2E','FENGSHEN_CENTER_RUNTIME_HTTP_ROOT','FENGSHEN_CENTER_RUNTIME_HTTP_REPO',
    'FENGSHEN_CENTER_RUNTIME_HTTP_FACE_JPEG','FENGSHEN_CENTER_RUNTIME_HTTP_PROFILE_JPEG','FENGSHEN_CENTER_RUNTIME_HTTP_BATCH','FENGSHEN_CENTER_RUNTIME_HTTP_STREAM','FENGSHEN_CENTER_RUNTIME_HTTP_ASR',
    'FENGSHEN_CENTER_RUNTIME_HTTP_VAD','FENGSHEN_CENTER_RUNTIME_HTTP_SPEECH_WAV',
    'FENGSHEN_CENTER_E2E','FENGSHEN_CENTER_BROWSER_E2E','FENGSHEN_CENTER_STARTUP_TIMEOUT_E2E',
    'FENGSHEN_LLAMA_REAL_INFERENCE','FENGSHEN_WHISPER_REAL_MANIFEST','FENGSHEN_TEST_REAL_HAAR')
$saved = @{}
foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
function Native([string]$Program, [string[]]$Arguments) {
    & $Program @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Native command failed with exit code $LASTEXITCODE." }
}
try {
    $env:GOTOOLCHAIN = 'local'
    $env:GOPROXY = 'off'
    $env:GOSUMDB = 'off'
    $env:GOWORK = 'off'
    $env:CGO_ENABLED = '0'
    $env:GOFLAGS = '-mod=readonly'
    # Build's default test pass must not inherit any explicit E2E opt-ins.
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_E2E = '0'
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_BATCH = '0'
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_STREAM = '0'
    $env:FENGSHEN_CENTER_E2E = '0'
    $env:FENGSHEN_CENTER_BROWSER_E2E = '0'
    $env:FENGSHEN_CENTER_STARTUP_TIMEOUT_E2E = '0'
    $env:FENGSHEN_LLAMA_REAL_INFERENCE = '0'
    $env:FENGSHEN_WHISPER_REAL_MANIFEST = ''
    $env:FENGSHEN_TEST_REAL_HAAR = '0'
    $build = @{ Output = $out }
    if ($BuildWhisperBatch) {
        $build.BuildWhisperBatch = $true
        $build.BuildWhisperBatchDependencyRoot = $WhisperBatchDependencyRoot
    }
    if ($BuildWhisperStream) {
        $build.BuildWhisperStream = $true
        $build.WhisperStreamDependencyRoot = $WhisperStreamDependencyRoot
    }
    & (Join-Path $PSScriptRoot 'build_model_center.ps1') @build
    $go = (Get-Command go -CommandType Application | Select-Object -First 1).Source
    $driver = Join-Path $out 'runtime-http-driver.exe'
    Native $go @('-C', $module, 'test', '-c', '-o', $driver, '.')
    $center = Join-Path $out 'model-center\fengshen-model-center.exe'
    $delivery = Join-Path $out 'model-center\delivery.json'
    $centerSHA = (Get-FileHash -LiteralPath $center -Algorithm SHA256).Hash.ToLowerInvariant()
    $deliverySHA = (Get-FileHash -LiteralPath $delivery -Algorithm SHA256).Hash.ToLowerInvariant()
    $template = Join-Path $out 'runtime-http-engine-template.json'
    $doc = @{ schema = 1; engines = @(@{ inputpath = 'runtime-http-driver.exe'; edition = 'developer'; management = $true }) }
    [IO.File]::WriteAllText($template, (ConvertTo-Json -InputObject $doc -Depth 8), [Text.UTF8Encoding]::new($false))
    # Use the production trust builder; never synthesize engine trust digests.
    Native $go @('-C', $module, 'run', './build', '-mode', 'engine-trust', '-root', $out, '-template', $template, '-center-sha256', $centerSHA, '-delivery-sha256', $deliverySHA)
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_ROOT = $out
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_REPO = $repo
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_FACE_JPEG = $FaceJpeg
    # Never inherit a profile input: only this invocation's explicit parameter opts in.
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_PROFILE_JPEG = $ProfileJpeg
    if ($ProfileJpeg) {
        $profileSHA = (Get-FileHash -LiteralPath $ProfileJpeg -Algorithm SHA256).Hash.ToLowerInvariant()
        Write-Output "Profile requested (not yet verified): source=$ProfileJpeg sha256=$profileSHA. Genuine-source visual review is required; a mirrored derivative is not an independent sample."
    } else {
        Write-Output 'Profile HTTP NOT RUN: no explicit -ProfileJpeg; frontal/no_face success does not cover profile.'
    }
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_BATCH = '0'
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_STREAM = '0'
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_ASR = ''
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_VAD = ''
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_SPEECH_WAV = ''
    if ($BuildWhisperBatch) { $env:FENGSHEN_CENTER_RUNTIME_HTTP_BATCH = '1' }
    if ($BuildWhisperBatch -or $BuildWhisperStream) {
        $env:FENGSHEN_CENTER_RUNTIME_HTTP_ASR = $ASR
        $env:FENGSHEN_CENTER_RUNTIME_HTTP_SPEECH_WAV = $SpeechWav
    }
    if ($BuildWhisperStream) {
        $env:FENGSHEN_CENTER_RUNTIME_HTTP_STREAM = '1'
        $env:FENGSHEN_CENTER_RUNTIME_HTTP_VAD = $VAD
    }
    $env:FENGSHEN_CENTER_RUNTIME_HTTP_E2E = '1'
    Write-Output "Fresh external delivery and isolated state root: $out"
    Native $driver @('-test.run', '^TestRuntimeHTTPE2E$', '-test.v', '-test.timeout', '45m')
    Write-Output 'Real HTTP suite passed only for the capabilities explicitly requested. Metadata/file presence alone is not inference evidence. Clean environment and user-engine business authorization are not verified by this suite.'
} finally {
    foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') }
    # Keep only this unique run's artifacts for inspection. Never kill by name/port,
    # remove weights, alter old registrations, or clean other builds.
}
