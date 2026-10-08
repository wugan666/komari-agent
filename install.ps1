# Windows PowerShell installation script for Komari Agent

# Logging functions with colors
function Log-Info { param([string]$Message) Write-Host "$Message"    -ForegroundColor Cyan }
function Log-Success { param([string]$Message) Write-Host "$Message"    -ForegroundColor Green }
function Log-Warning { param([string]$Message) Write-Host "[WARNING] $Message"    -ForegroundColor Yellow }
function Log-Error { param([string]$Message) Write-Host "[ERROR] $Message"    -ForegroundColor Red }
function Log-Step { param([string]$Message) Write-Host "$Message"    -ForegroundColor Magenta }
function Log-Config { param([string]$Message) Write-Host "- $Message"    -ForegroundColor White }

# Default parameters
$InstallDir = Join-Path $Env:ProgramFiles "Komari"
$ServiceName = "komari-agent"
$GitHubProxy = ""
$KomariArgs = @()
$InstallVersion = ""
$ReleaseRepository = "wugan666/komari-agent"

# Parse script arguments
for ($i = 0; $i -lt $args.Count; $i++) {
    switch ($args[$i]) {
        "--install-dir" { $InstallDir = $args[$i + 1]; $i++; continue }
        "--install-service-name" { $ServiceName = $args[$i + 1]; $i++; continue }
        "--install-ghproxy" { $GitHubProxy = $args[$i + 1]; $i++; continue }
        "--install-version" { $InstallVersion = $args[$i + 1]; $i++; continue }
        Default { $KomariArgs += $args[$i] }
    }
}

# Ensure running as Administrator
if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
    ).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)) {
    Log-Error "Please run this script as Administrator."
    exit 1
}

# Prepare GitHub proxy display
if ($GitHubProxy -ne '') {
    if ($GitHubProxy -notmatch '^https://') {
        Log-Error "GitHub proxy must use HTTPS."
        exit 1
    }
    $GitHubProxy = $GitHubProxy.TrimEnd('/')
    $ProxyDisplay = $GitHubProxy
    Log-Warning "The explicitly selected proxy can replace both binaries and checksums. Use only a proxy you trust."
} else { $ProxyDisplay = '(direct)' }

# Detect architecture early for constructing binary name
switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    'x86' { $arch = '386' }
    Default { Log-Error "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE"; exit 1 }
}

# Ensure installation directory exists for nssm and agent
Log-Step "Ensuring installation directory exists: $InstallDir"
New-Item -ItemType Directory -Path $InstallDir -Force -ErrorAction SilentlyContinue | Out-Null # Ensure $InstallDir exists

# Check for nssm and download if not present
$nssmExeToUse = Join-Path $InstallDir "nssm.exe"

# First, check if nssm is in PATH and is functional
$nssmCmd = Get-Command nssm -ErrorAction SilentlyContinue
if ($nssmCmd) {
    Log-Info "nssm found in PATH at $($nssmCmd.Source)."
    try {
        $nssmVersionOutput = nssm version 2>&1
        Log-Info "Detected nssm version: $nssmVersionOutput"
    }
    catch {
        Log-Warning "nssm found in PATH failed to execute 'nssm version'. Will attempt to use/download local copy. Error: $_"
        $nssmCmd = $null # Force re-evaluation for local copy or download
    }
}

# If nssm not found in PATH or the one in PATH failed, check local $InstallDir
if (-not $nssmCmd) {
    if (Test-Path $nssmExeToUse) {
        Log-Info "nssm found at $nssmExeToUse. Attempting to use it by adding $InstallDir to PATH."
        $env:Path = "$($InstallDir);$($env:Path)"
        $nssmCmd = Get-Command nssm -ErrorAction SilentlyContinue
        if ($nssmCmd) {
            try {
                $nssmVersionOutput = nssm version 2>&1
            }
            catch {
                Log-Warning "nssm from $InstallDir failed to execute 'nssm version'. Error: $_"
                $nssmCmd = $null # Mark as unusable
            }
        }
        else {
            Log-Warning "Failed to make nssm from $nssmExeToUse available via PATH. Will attempt download."
        }
    }
}

# If still no usable nssm command, proceed to download
if (-not $nssmCmd) {
    Log-Info "nssm not found or not usable. Attempting to download to $InstallDir..."
    $NssmVersion = "2.24"
    $NssmZipUrl = "https://nssm.cc/release/nssm-$NssmVersion.zip"
    # 固定官方 2.24 压缩包哈希，在解压或执行之前验证。
    $NssmZipSha256 = "727d1e42275c605e0f04aba98095c38a8e1e46def453cdffce42869428aa6743"
    $NssmTempParent = [IO.Path]::GetFullPath($env:TEMP).TrimEnd([IO.Path]::DirectorySeparatorChar)
    $NssmTempRoot = Join-Path $NssmTempParent ("komari-nssm-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $NssmTempRoot -ErrorAction Stop | Out-Null
    $TempNssmZipPath = Join-Path $NssmTempRoot "nssm-$NssmVersion.zip"
    $TempExtractDir = Join-Path $NssmTempRoot "extracted"

    try {
        Log-Info "Downloading nssm from $NssmZipUrl..."
        Invoke-WebRequest -Uri $NssmZipUrl -OutFile $TempNssmZipPath -UseBasicParsing -TimeoutSec 120 -ErrorAction Stop
        if ((Get-FileHash -LiteralPath $TempNssmZipPath -Algorithm SHA256 -ErrorAction Stop).Hash -ine $NssmZipSha256) {
            throw "NSSM archive SHA256 verification failed."
        }

        New-Item -ItemType Directory -Path $TempExtractDir -ErrorAction Stop | Out-Null
        Expand-Archive -LiteralPath $TempNssmZipPath -DestinationPath $TempExtractDir -ErrorAction Stop
        
        $NssmSourceDirInsideZip = "nssm-$NssmVersion" # Used for Get-ChildItem search path
        # The path part within the extracted nssm folder, e.g., "nssm-2.24\win32"
        # 'win32' nssm is used for both 'amd64' and 'arm64' PowerShell architectures.
        $NssmArchSubDir = Join-Path "nssm-$NssmVersion" "win32"
        $NssmSourceExePath = Join-Path (Join-Path $TempExtractDir $NssmArchSubDir) "nssm.exe"

        if (-not (Test-Path $NssmSourceExePath)) {
            Log-Error "Could not find nssm.exe at expected path: $NssmSourceExePath after extraction."
            # Fallback search for nssm.exe within the extracted directory
            $foundNssmFallback = Get-ChildItem -Path $TempExtractDir -Recurse -Filter "nssm.exe" | 
            Where-Object { $_.FullName -like "*$NssmArchSubDir\nssm.exe" } | 
            Select-Object -First 1
            if ($foundNssmFallback) {
                Log-Warning "Found nssm.exe at $($foundNssmFallback.FullName) using fallback search. Using this."
                $NssmSourceExePath = $foundNssmFallback.FullName
            }
            else {
                Log-Error "nssm.exe ($NssmArchSubDir) still not found in $TempExtractDir. Please install nssm manually (from https://nssm.cc) and ensure it's in your PATH."
                exit 1
            }
        }
        
        Copy-Item -Path $NssmSourceExePath -Destination $nssmExeToUse -Force

        $env:Path = "$($InstallDir);$($env:Path)"
        $nssmCmd = Get-Command nssm -ErrorAction SilentlyContinue # Re-check after adding to PATH
        if ($nssmCmd) {
            Log-Success "Downloaded nssm is now configured and available in PATH."
        }
        else {
            Log-Error "Failed to configure downloaded nssm in PATH from $nssmExeToUse. Please ensure $InstallDir is in your system PATH or nssm is installed globally."
            exit 1
        }
    }
    catch {
        Log-Error "Failed to download or configure nssm: $_"
        Log-Error "Please install nssm manually from https://nssm.cc and ensure nssm.exe is in your PATH."
        exit 1
    }
    finally {
        if (Test-Path -LiteralPath $NssmTempRoot) {
            $ResolvedNssmTempRoot = (Resolve-Path -LiteralPath $NssmTempRoot).Path
            if ((Split-Path -Parent $ResolvedNssmTempRoot) -eq $NssmTempParent -and
                (Split-Path -Leaf $ResolvedNssmTempRoot) -match '^komari-nssm-[a-f0-9]{32}$') {
                Remove-Item -LiteralPath $ResolvedNssmTempRoot -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }
}

# Final check that nssm is operational
try {
    $nssmVersionOutput = nssm version 2>&1
}
catch {
    Log-Error "nssm command failed to execute even after setup attempts. Please check the nssm installation and PATH. Error: $_"
    exit 1
}

Log-Step "Installation configuration:"
Log-Config "Service name: $ServiceName"
Log-Config "Install directory: $InstallDir"
Log-Config "GitHub proxy: $ProxyDisplay"
Log-Config "Release repository: $ReleaseRepository"
Log-Config "Agent arguments: $($KomariArgs -join ' ')"
if ($InstallVersion -ne "") {
    Log-Config "Specified agent version: $InstallVersion"
} else {
    Log-Config "Agent version: Latest"
}

# Paths
$BinaryName = "komari-agent-windows-$arch.exe"
$AgentPath = Join-Path $InstallDir "komari-agent.exe"

# Uninstall previous service and binary
function Uninstall-Previous {
    Log-Step "Checking for existing service..."
    # Check if service exists using nssm status, as Get-Service might not work for nssm services if not properly registered
    $serviceStatus = nssm status $ServiceName 2>&1
    if ($serviceStatus -notmatch "SERVICE_STOPPED" -and $serviceStatus -notmatch "does not exist") {
        Log-Info "Stopping service $ServiceName..."
        nssm stop $ServiceName 2>&1 | Out-Null
    }
    # Attempt to remove the service using nssm
    # We check if it exists first by trying to get its status.
    # nssm remove will succeed if the service exists, and fail otherwise.
    # We add confirm to avoid interactive prompts.
    $removeOutput = nssm remove $ServiceName confirm 2>&1
    if ($LASTEXITCODE -eq 0) {
    }
    elseif ($removeOutput -match "Can't open service! (The specified service does not exist as an installed service.)" -or $removeOutput -match "No such service" -or $removeOutput -match "does not exist") {
        Log-Info "Service $ServiceName does not exist or was already removed."
    }
    else {
        # If nssm remove fails for other reasons, try sc.exe delete as a fallback for older installations
        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($svc) {
            Stop-Service $ServiceName -Force -ErrorAction SilentlyContinue
            sc.exe delete $ServiceName | Out-Null
        }
    }

    # 保留旧二进制，验证成功后才使用新文件替换。
}

function Assert-AssetChecksum {
    param(
        [Parameter(Mandatory = $true)][string]$BinaryPath,
        [Parameter(Mandatory = $true)][string]$ChecksumPath,
        [Parameter(Mandatory = $true)][string]$AssetName
    )

    $ChecksumText = Get-Content -LiteralPath $ChecksumPath -Raw -ErrorAction Stop
    $ChecksumPattern = '\A([0-9a-fA-F]{64})[ \t]+\*?' + [regex]::Escape($AssetName) + '(?:\r?\n)?\z'
    $ChecksumMatch = [regex]::Match($ChecksumText, $ChecksumPattern)
    if (-not $ChecksumMatch.Success) {
        throw "Invalid SHA256 record for $AssetName."
    }
    $ActualHash = (Get-FileHash -LiteralPath $BinaryPath -Algorithm SHA256 -ErrorAction Stop).Hash
    if ($ActualHash -ine $ChecksumMatch.Groups[1].Value) {
        throw "SHA256 verification failed for $AssetName."
    }
}

function Get-LatestSnapshotVersion {
    param([Parameter(Mandatory = $true)][string]$AssetName)

    $ApiUrl = "https://api.github.com/repos/$ReleaseRepository/releases?per_page=100"
    $ApiUrls = @($ApiUrl)
    if ($GitHubProxy -ne "") {
        $ApiUrls = @("$GitHubProxy/$ApiUrl", $ApiUrl)
    }

    for ($i = 0; $i -lt $ApiUrls.Count; $i++) {
        try {
            Log-Info "Fetching snapshot releases from GitHub API..."
            $releases = Invoke-RestMethod -Uri $ApiUrls[$i] -UseBasicParsing -TimeoutSec 60 -ErrorAction Stop
        }
        catch {
            $releases = $null
        }

        if ($releases) {
            $latestSnapshot = $releases |
            Where-Object {
                $_.draft -eq $false -and
                $_.prerelease -eq $true -and
                $_.tag_name -like "Snapshot-*" -and
                (@($_.assets.name) -contains $AssetName) -and
                (@($_.assets.name) -contains "$AssetName.sha256")
            } |
            Sort-Object -Property @{ Expression = { [datetime]$_.published_at }; Descending = $true }, @{ Expression = { $_.tag_name }; Descending = $true } |
            Select-Object -First 1

            if ($latestSnapshot) {
                return $latestSnapshot.tag_name
            }
        }

        if ($i -lt ($ApiUrls.Count - 1)) {
            Log-Warning "Failed to resolve snapshot releases through GitHub proxy, retrying directly."
        }
    }

    throw "No snapshot release contains asset $AssetName."
}

$versionToInstall = ""
if ($InstallVersion -ne "" -and $InstallVersion -ine "latest") {
    Log-Info "Attempting to install specified version: $InstallVersion"
    if ($InstallVersion -ieq "snapshot") {
        Log-Info "Resolving the latest snapshot version..."
        try {
            $versionToInstall = Get-LatestSnapshotVersion -AssetName $BinaryName
            Log-Success "Latest snapshot version fetched: $versionToInstall"
        }
        catch {
            Log-Error "Failed to resolve the latest snapshot version: $_"
            exit 1
        }
    }
    else {
        $versionToInstall = $InstallVersion
    }
}
else {
    $ApiUrl = "https://api.github.com/repos/$ReleaseRepository/releases/latest"
    try {
        Log-Step "Fetching latest release version from GitHub API..."
        if ($GitHubProxy) {
            try {
                $release = Invoke-RestMethod -Uri "$GitHubProxy/$ApiUrl" -UseBasicParsing -TimeoutSec 60 -ErrorAction Stop
            }
            catch {
                Log-Warning "Failed to resolve latest release through GitHub proxy, retrying directly."
                $release = Invoke-RestMethod -Uri $ApiUrl -UseBasicParsing -TimeoutSec 60 -ErrorAction Stop
            }
        } else {
            $release = Invoke-RestMethod -Uri $ApiUrl -UseBasicParsing -TimeoutSec 60 -ErrorAction Stop
        }
        $versionToInstall = $release.tag_name
        Log-Success "Latest version fetched: $versionToInstall"
    }
    catch {
        Log-Error "Failed to fetch latest version: $_"
        exit 1
    }
}
if ($versionToInstall -cnotmatch '^[A-Za-z0-9_+-][A-Za-z0-9._+-]*$') {
    Log-Error "Invalid release tag: $versionToInstall"
    exit 1
}
Log-Success "Installing Komari Agent version: $versionToInstall"

# Construct download URL
$BinaryName = "komari-agent-windows-$arch.exe"
$DownloadUrl = if ($GitHubProxy) { "$GitHubProxy/https://github.com/$ReleaseRepository/releases/download/$versionToInstall/$BinaryName" } else { "https://github.com/$ReleaseRepository/releases/download/$versionToInstall/$BinaryName" }

# Download and install
New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
Log-Info "URL: $DownloadUrl"
$StageDir = Join-Path $InstallDir (".komari-install-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $StageDir -ErrorAction Stop | Out-Null
$StagedBinary = Join-Path $StageDir $BinaryName
$StagedChecksum = Join-Path $StageDir "$BinaryName.sha256"
try {
    # 下载和校验全部完成前，不停止旧服务、不覆盖旧二进制。
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $StagedBinary -UseBasicParsing -TimeoutSec 300 -ErrorAction Stop
    if ((Get-Item -LiteralPath $StagedBinary -ErrorAction Stop).Length -eq 0) {
        throw "Downloaded agent binary is empty."
    }
    Invoke-WebRequest -Uri "$DownloadUrl.sha256" -OutFile $StagedChecksum -UseBasicParsing -TimeoutSec 60 -ErrorAction Stop
    Assert-AssetChecksum -BinaryPath $StagedBinary -ChecksumPath $StagedChecksum -AssetName $BinaryName
    Log-Success "SHA256 verification passed."
    Uninstall-Previous
    if (Test-Path -LiteralPath $AgentPath) {
        [IO.File]::Replace($StagedBinary, $AgentPath, [NullString]::Value)
    } else {
        [IO.File]::Move($StagedBinary, $AgentPath)
    }
}
catch {
    Log-Error "Download, verification, or replacement failed: $_"
    exit 1
}
finally {
    Remove-Item -LiteralPath $StagedBinary, $StagedChecksum -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $StageDir -Force -ErrorAction SilentlyContinue
}
Log-Success "Downloaded and saved to $AgentPath"

# Register and start service
Log-Step "Configuring Windows service with nssm..."
$argString = $KomariArgs -join ' '
# Ensure InstallDir and AgentPath are quoted if they contain spaces
$quotedAgentPath = "`"$AgentPath`""
nssm install $ServiceName $quotedAgentPath $argString
# Set display name and startup type using nssm
nssm set $ServiceName DisplayName "Komari Agent Service"
nssm set $ServiceName Start SERVICE_AUTO_START
nssm set $ServiceName AppExit Default Restart
nssm set $ServiceName AppRestartDelay 5000
# Start the service using nssm
nssm start $ServiceName
Log-Success "Service $ServiceName installed and started using nssm."

Log-Success "Komari Agent installation completed!"
Log-Config "Service name: $ServiceName"
Log-Config "Arguments: $argString"
