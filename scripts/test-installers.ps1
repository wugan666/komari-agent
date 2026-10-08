$ErrorActionPreference = 'Stop'

# 仅提取校验函数，不执行安装器的管理员、网络或服务操作。
$InstallerPath = Join-Path $PSScriptRoot '../install.ps1'
$Tokens = $null
$ParseErrors = $null
$InstallerAst = [System.Management.Automation.Language.Parser]::ParseFile(
    $InstallerPath, [ref]$Tokens, [ref]$ParseErrors
)
if ($ParseErrors.Count) { throw ($ParseErrors | Out-String) }
$ChecksumFunction = $InstallerAst.Find({
    param($Node)
    $Node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
    $Node.Name -eq 'Assert-AssetChecksum'
}, $false)
if (-not $ChecksumFunction) { throw 'Checksum verification function not found.' }
. ([scriptblock]::Create($ChecksumFunction.Extent.Text))

$TestDir = Join-Path ([IO.Path]::GetTempPath()) ('komari-installer-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TestDir | Out-Null
$TestName = 'komari-agent-windows-amd64.exe'
$TestBinary = Join-Path $TestDir $TestName
$TestChecksum = "$TestBinary.sha256"
$FixtureHash = 'e09932a21e68c61339c0a8db027bc45d2bd91bed1e801f4d8054d5bedc0f38b2'
$AgentPath = Join-Path $TestDir 'agent.exe'
$StagedBinary = Join-Path $TestDir 'staged-agent.exe'
$StagedChecksum = "$StagedBinary.sha256"

function Expect-Rejected {
    param([string]$Case)
    $Rejected = $false
    try {
        Assert-AssetChecksum -BinaryPath $TestBinary -ChecksumPath $TestChecksum -AssetName $TestName
    } catch { $Rejected = $true }
    if (-not $Rejected) { throw "FAIL: accepted $Case" }
}

try {
    [IO.File]::WriteAllText($TestBinary, "owned release`n")
    [IO.File]::WriteAllText($TestChecksum, "$FixtureHash  $TestName`n")
    Assert-AssetChecksum -BinaryPath $TestBinary -ChecksumPath $TestChecksum -AssetName $TestName
    [IO.File]::WriteAllText($TestChecksum, "$FixtureHash *$TestName`r`n")
    Assert-AssetChecksum -BinaryPath $TestBinary -ChecksumPath $TestChecksum -AssetName $TestName

    [IO.File]::WriteAllText($TestBinary, "tampered release`n")
    Expect-Rejected 'modified binary'
    [IO.File]::WriteAllText($TestBinary, "owned release`n")
    [IO.File]::WriteAllText($TestChecksum, "$FixtureHash  $TestName.other`n")
    Expect-Rejected 'wrong filename'
    [IO.File]::WriteAllText($TestChecksum, "$FixtureHash  $TestName`n$FixtureHash  $TestName`n")
    Expect-Rejected 'duplicate checksum records'
    [IO.File]::WriteAllText($TestChecksum, "invalid  $TestName`n")
    Expect-Rejected 'malformed hash'
    [IO.File]::WriteAllText($TestChecksum, '')
    Expect-Rejected 'empty checksum'
    Remove-Item -LiteralPath $TestChecksum
    Expect-Rejected 'missing checksum'
    Write-Output 'PASS: PowerShell installer checksum verification (8 cases)'

    # 复用实际安装事务主体，以测试桩隔离外网和服务，验证失败不能影响旧程序。
    $DownloadTransaction = $InstallerAst.EndBlock.Statements | Where-Object {
        $_ -is [System.Management.Automation.Language.TryStatementAst] -and
        $_.Body.Extent.Text -match 'Assert-AssetChecksum -BinaryPath \$StagedBinary'
    } | Select-Object -First 1
    if (-not $DownloadTransaction) { throw 'Download transaction not found.' }
    $TransactionText = $DownloadTransaction.Body.Extent.Text
    $TransactionBody = [scriptblock]::Create($TransactionText.Substring(1, $TransactionText.Length - 2))
    function Log-Success { param([string]$Message) }
    function Uninstall-Previous { $script:ServiceStopped = $true }
    function Invoke-WebRequest {
        [CmdletBinding()]
        param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing, [int]$TimeoutSec)
        if ($Uri.EndsWith('.sha256')) {
            if ($script:MockMode -eq 'missing') { throw 'Mock HTTP 404' }
            $DownloadedHash = if ($script:MockMode -eq 'mismatch') { '0' * 64 } else { $FixtureHash }
            [IO.File]::WriteAllText($OutFile, "$DownloadedHash  $TestName`n")
        } else {
            [IO.File]::WriteAllText($OutFile, "owned release`n")
        }
    }
    $BinaryName = $TestName
    $DownloadUrl = "https://github.com/wugan666/komari-agent/releases/download/v1.0.0-owned.1/$TestName"
    foreach ($script:MockMode in @('missing', 'mismatch', 'valid')) {
        [IO.File]::WriteAllText($AgentPath, "existing release`n")
        $script:ServiceStopped = $false
        $Failed = $false
        $TransactionError = $null
        try { . $TransactionBody } catch { $Failed = $true; $TransactionError = $_ }
        $InstalledContent = [IO.File]::ReadAllText($AgentPath)
        if ($script:MockMode -eq 'valid') {
            if ($Failed -or -not $script:ServiceStopped -or $InstalledContent -cne "owned release`n") {
                throw "Valid release was not installed after verification: $TransactionError"
            }
        } elseif (-not $Failed -or $script:ServiceStopped -or $InstalledContent -cne "existing release`n") {
            throw "Verification failure affected existing installation ($script:MockMode)."
        }
    }
    Write-Output 'PASS: PowerShell installer replacement ordering (3 cases)'
} finally {
    Remove-Item -LiteralPath $TestBinary, $TestChecksum, $AgentPath, $StagedBinary, $StagedChecksum -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $TestDir -Force
}
