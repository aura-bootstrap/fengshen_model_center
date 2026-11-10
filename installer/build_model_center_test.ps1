# Offline contract checks; does not invoke make_all or create build products.
$ErrorActionPreference = 'Stop'
$scriptPath = Join-Path $PSScriptRoot 'build_model_center.ps1'
$tokens=$null; $errors=$null
$ast = [Management.Automation.Language.Parser]::ParseFile($scriptPath,[ref]$tokens,[ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
$source = [IO.File]::ReadAllText($scriptPath)
foreach ($required in @(
    "[string[]]`$Protocols = @()",
    "fengshen-shim.exe",
    "[string]`$MetadataArgument = '--component-info'",
    "protocols=`$Protocols",
    "arguments=@(`$MetadataArgument)",
    "Metadata-Smoke 'haar-consumer' `$haarDir 'fengshen-model-center.exe' 'go' '--haar-component-info' @('haar/v1')",
    "-BuildInputs `$centerBuildInputs -ModelRefs @('haar-frontal','haar-profile')",
    "'fengshen.demucs.cli/v1'",
    "'fengshen.seedvc.cli/v1'",
    "@('demucs','demucs-ft')",
    "@('seed-vc','seed-whisper','campplus','bigvgan')",
    "Copy-Item -LiteralPath (Join-Path `$centerDir 'fengshen-model-center.exe') -Destination (Join-Path `$haarDir 'fengshen-model-center.exe')",
    '[switch]$BuildEngines',
    '-X main.buildVersion=$Version',
    "[string]`$StartupReportRef = 'startup-report.json'",
    'startup_report_ref=$StartupReportRef',
    'model-center\components\model-center\',
    '-X github.com/aura-bootstrap/fengshen_model_center/integration.CenterSHA256=$centerSHA',
    '-X github.com/aura-bootstrap/fengshen_model_center/integration.DeliverySHA256=$deliverySHA',
    "' -X fengshen-slicer/internal/build.Flavor=internal'",
    "startup_test='passed'; metadata_only=",
    "clean_environment='not_verified'",
    'protocol_evidence=$protocolEvidence',
    "'declared_not_tested' } else { 'not_declared' }",
    '$info.version -cne $Version',
    "'snapshot', '-root', $" + 'repo',
    "'verify-snapshot', '-root', $" + 'repo',
    "'test', './...'",
    "'vet', './...'"
)) {
    if (-not $source.Contains($required)) { throw "Missing build contract: $required" }
}
foreach ($forbidden in @('pending_integration_build','component_info_passed','DevelopmentKey','make_all.ps1','git commit','git push')) {
    if ($source.Contains($forbidden)) { throw "Forbidden pipeline contract: $forbidden" }
}
# No engine is built unless the explicit switch gates that block.
$gates = @($ast.FindAll({param($n) $n -is [Management.Automation.Language.IfStatementAst] -and $n.Clauses[0].Item1.Extent.Text -eq '$BuildEngines'},$true))
if ($gates.Count -ne 3) { throw 'Expected explicit engine input and engine build gates.' }
# Parse/source contracts only: never dot-source the build or execute third-party tools.
if (-not @($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'BuildWhisperBatch' -and $_.StaticType -eq [switch] }).Count) { throw 'Missing explicit Whisper switch.' }
if (-not $source.Contains('[string]$BuildWhisperBatchDependencyRoot')) { throw 'Missing fresh native batch dependency parameter.' }
foreach ($required in @('build_whisper_batch.ps1', '$BuildWhisperBatchDependencyRoot', '4979e04-batch-v2', 'Removed the private stock whisper-cli build')) {
    if (-not $source.Contains($required)) { throw "Missing batch integration: $required" }
}
if ($source.Contains("--target','whisper-cli")) { throw 'Stock CLI must not deliver a runtime component.' }
$functions = @{}
foreach ($f in $ast.FindAll({param($n) $n -is [Management.Automation.Language.FunctionDefinitionAst]},$true)) { $functions[$f.Name] = $f.Body.Extent.Text }
foreach ($required in @('[switch]$BuildWhisperStream','[switch]$BuildLlamaRuntime','build_whisper_stream.ps1','build_llama_runtime.ps1',"Move-Item -LiteralPath `$from -Destination `$destination",'Shared facehaar is embedded','same-byte center copy')) {
    if (-not $source.Contains($required)) { throw "Missing native/embedded integration gate: $required" }
}
foreach ($required in @("[ValidateSet('baseline','avx2')][string]`$LlamaProfile = 'baseline'", "'-Profile',`$LlamaProfile", '$nativeLock.profiles.$LlamaProfile.version')) {
    if (-not $source.Contains($required)) { throw "Missing explicit llama profile integration: $required" }
}
$batchPath = Join-Path $PSScriptRoot 'build_whisper_batch.ps1'
$batchTokens=$null; $batchErrors=$null
$null = [Management.Automation.Language.Parser]::ParseFile($batchPath,[ref]$batchTokens,[ref]$batchErrors)
if ($batchErrors.Count) { throw ($batchErrors | Out-String) }
$batch = [IO.File]::ReadAllText($batchPath)
foreach ($required in @('Build directory must not exist', 'whisper-stream\main.cpp', 'batch_test.cpp', "@('--component-info')", 'Native batch metadata mismatch', 'fengshen.whispercpp.batch/v1', 'Unclosed non-system DLL dependency', "-or -not `$info.vad_supported", "vad_supported=`$true", "model_refs=@('whisper-asr','whisper-vad')", "clean_environment='not_verified'", "inference='not_tested'", 'metadata-smoke.json', 'delivery-index.json')) {
    if (-not $batch.Contains($required)) { throw "Missing strict batch builder contract: $required" }
}
if ($batch.Contains("vad_supported=`$false") -or $batch.Contains("model_refs=@('whisper-asr')") -or $batch.Contains("@('--metadata')")) { throw 'Batch must declare native VAD without using the stream metadata entry.' }
$streamPath = Join-Path $PSScriptRoot 'build_whisper_stream.ps1'
$streamTokens=$null; $streamErrors=$null
$streamAst = [Management.Automation.Language.Parser]::ParseFile($streamPath,[ref]$streamTokens,[ref]$streamErrors)
if ($streamErrors.Count) { throw ($streamErrors | Out-String) }
$stream = [IO.File]::ReadAllText($streamPath)
foreach ($required in @('Build directory must not exist','W:\QoderCN\','Native metadata identity/protocol/VAD mismatch',"@('--metadata')",'fengshen.whispercpp.stream/v1','Unclosed non-system DLL dependency','manifest.json','startup-report.json','delivery-index.json',"'-j', '4'","vad_supported=`$true","inference='not_tested'")) {
    if (-not $stream.Contains($required)) { throw "Missing strict native stream gate: $required" }
}
$streamManifests = @($streamAst.FindAll({param($n) $n -is [Management.Automation.Language.HashtableAst] -and @($n.KeyValuePairs | Where-Object { $_.Item1.Extent.Text -ceq 'component_id' -and $_.Item2.Extent.Text -ceq "'whisper-stream'" }).Count -eq 1 -and @($n.KeyValuePairs | Where-Object { $_.Item1.Extent.Text -ceq 'protocols' }).Count -eq 1},$true))
if ($streamManifests.Count -ne 1) { throw 'Expected one native stream manifest declaration.' }
$protocolValue = @($streamManifests[0].KeyValuePairs | Where-Object { $_.Item1.Extent.Text -ceq 'protocols' })[0].Item2
$streamProtocols = @($protocolValue.FindAll({param($n) $n -is [Management.Automation.Language.StringConstantExpressionAst]},$true) | ForEach-Object { $_.Value })
if ($streamProtocols.Count -ne 2 -or $streamProtocols[0] -cne 'fengshen.whispercpp.stream/v1' -or $streamProtocols[1] -cne '--fengshen-stream-v1') { throw 'Native stream manifest must declare both protocol and executable entry flag.' }
# Inspect helper and Haar commands, not comments, for executable delivery wiring.
foreach ($required in @('$MetadataArgument)', 'arguments=@($MetadataArgument)', '$info.embedded -isnot [bool]', '$info.version -cne $Version', '$declaredProtocols[$i] -cne $Protocols[$i]', "'declared_not_tested'", 'protocol_evidence=$protocolEvidence')) {
    if (-not $functions['Metadata-Smoke'].Contains($required)) { throw "Missing metadata execution contract: $required" }
}
$haarCommands = @($ast.FindAll({param($n) $n -is [Management.Automation.Language.CommandAst] -and $n.GetCommandName() -in @('Manifest','Metadata-Smoke') -and $n.CommandElements[1].Extent.Text -eq "'haar-consumer'"},$true))
if ($haarCommands.Count -ne 1 -or $haarCommands[0].GetCommandName() -cne 'Metadata-Smoke') { throw 'Haar must generate its manifest only after actual metadata execution.' }
$metadataSteps = @("JsonFile (Join-Path `$Directory 'component-info.json')", "Manifest `$Id `$Directory `$Entry `$Packaging `$BuildInputs `$ModelRefs 'startup-report.json' `$Protocols", "JsonFile (Join-Path `$Directory 'startup-report.json')")
$position = 0
foreach ($step in $metadataSteps) {
    $next = $functions['Metadata-Smoke'].IndexOf($step, $position, [StringComparison]::Ordinal)
    if ($next -lt 0) { throw "Missing ordered metadata delivery gate: $step" }
    $position = $next + $step.Length
}
$frozenMetadata = @($ast.FindAll({param($n) $n -is [Management.Automation.Language.CommandAst] -and $n.GetCommandName() -eq 'Metadata-Smoke' -and $n.CommandElements[1].Extent.Text -eq '$id'},$true))
if ($frozenMetadata.Count -ne 1) { throw 'Frozen runners require one protocol-checked metadata delivery command.' }
foreach ($required in @('-Protocols @($protocol)', '-BuildInputs $pins', '-ModelRefs $modelRefs')) {
    if (-not $frozenMetadata[0].Extent.Text.Contains($required)) { throw "Missing frozen metadata delivery argument: $required" }
}
foreach ($name in @('dac','audiotools','matplotlib')) {
    if (-not $source.Contains("'--exclude-module','$name'")) { throw "Seed frozen analysis must exclude unsupported optional module: $name" }
}
if ($source.Contains("component_id='haar-consumer'; build='pending_build'")) { throw 'Embedded Haar must not remain pending.' }
# Execute only the parsed helpers against private files, never the build pipeline.
foreach ($name in @('RelativePath','JsonFile','Manifest','Metadata-Smoke')) {
    $definition = @($ast.FindAll({param($n) $n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true))
    if ($definition.Count -ne 1) { throw "Missing unique validation function: $name" }
    . ([scriptblock]::Create($definition[0].Extent.Text))
}
function Expect-Rejected([scriptblock]$Action, [string]$Message) {
    $rejected = $false
    try { & $Action | Out-Null } catch { $rejected = $true }
    if (-not $rejected) { throw "Validation unexpectedly accepted: $Message" }
}
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('center-build-contract-' + [Guid]::NewGuid().ToString('N'))
try {
    # Contract-only template capture: no compiler, EXE or startup report is produced.
    $Output = Join-Path $fixture 'templates'
    New-Item -ItemType Directory -Path $Output -Force | Out-Null
    $Version = 'contract-test'
    $go = 'contract-only-no-executable'
    $module = Join-Path $PSScriptRoot '..'
    function Run([string]$Program, [string[]]$Arguments) {
        if ($Arguments -notcontains 'manifest') { throw 'Contract test attempted a non-manifest build command.' }
    }
    Manifest 'model-center' $Output 'center.exe' 'go' @()
    $template = Get-Content -LiteralPath (Join-Path $Output 'model-center.template.json') -Raw | ConvertFrom-Json
    if (@($template.protocols).Count -ne 0 -or @($template.model_refs).Count -ne 0 -or $template.startup_report_ref -cne 'startup-report.json') { throw 'Default center manifest behavior changed.' }
    Manifest 'haar-consumer' $Output 'center.exe' 'go' @() @('haar-frontal','haar-profile') 'startup-report.json' @('haar/v1')
    $template = Get-Content -LiteralPath (Join-Path $Output 'haar-consumer.template.json') -Raw | ConvertFrom-Json
    if (@($template.protocols).Count -ne 1 -or $template.protocols[0] -cne 'haar/v1' -or ($template.model_refs -join ',') -cne 'haar-frontal,haar-profile') { throw 'Embedded Haar manifest contract lost protocols/model refs.' }
    foreach ($runner in @(
        @{id='demucs-runner'; protocol='fengshen.demucs.cli/v1'; models=@('demucs','demucs-ft')},
        @{id='seedvc-runner'; protocol='fengshen.seedvc.cli/v1'; models=@('seed-vc','seed-whisper','campplus','bigvgan')}
    )) {
        Manifest $runner.id $Output ($runner.id + '.exe') 'pyinstaller-onedir' @() $runner.models 'startup-report.json' @($runner.protocol)
        $template = Get-Content -LiteralPath (Join-Path $Output ($runner.id + '.template.json')) -Raw | ConvertFrom-Json
        if (@($template.protocols).Count -ne 1 -or $template.protocols[0] -cne $runner.protocol -or ($template.model_refs -join ',') -cne ($runner.models -join ',')) { throw 'Frozen manifest lost exact protocol/model references.' }
    }
    Expect-Rejected { Metadata-Smoke 'model-center' $Output 'missing.exe' 'go' '--haar-component-info' @('haar/v1') } 'Haar argument used for center'
    Expect-Rejected { Metadata-Smoke 'haar-consumer' $Output 'missing.exe' 'go' '--haar-component-info --root=outside' @('haar/v1') } 'multiple metadata arguments'
} finally {
    if (Test-Path -LiteralPath $fixture) { Remove-Item -LiteralPath $fixture -Recurse -Force }
}
Write-Output 'build_model_center installer contracts and isolated validation tests passed.'
