param(
    [string]$SecretsFile = "",
    [string]$Repo = ""
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new()

# 仓库根目录由 git 决定，脚本放在仓库内任何层级都能用
$RepoRoot = git rev-parse --show-toplevel
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($RepoRoot)) {
    throw "Not inside a git repository."
}
$RepoRoot = $RepoRoot.Trim()

if ([string]::IsNullOrWhiteSpace($SecretsFile)) {
    $SecretsFile = Join-Path $RepoRoot ".env.secrets"
}

if (-not (Test-Path -Path $SecretsFile)) {
    throw "Secrets file not found: $SecretsFile"
}

if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    throw "gh was not found. Install GitHub CLI first and run gh auth login."
}

# gh 是原生程序，$ErrorActionPreference 管不到它的退出码，必须显式检查
gh auth status | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "gh is not authenticated. Run: gh auth login -h github.com"
}

# 本仓库同时有 origin 和 upstream，不显式指定 gh 会拒绝执行。
# 固定写入 origin 指向的仓库，避免把密钥推到上游仓库。
if ([string]::IsNullOrWhiteSpace($Repo)) {
    $originUrl = git -C $RepoRoot remote get-url origin
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($originUrl)) {
        throw "Cannot read the origin remote. Pass -Repo owner/name explicitly."
    }

    $originUrl = $originUrl.Trim()
    $pattern = '^(?:https?://[^/]+/|git@[^:]+:|ssh://git@[^/]+/)(?<owner>[^/]+)/(?<name>[^/]+?)(?:\.git)?$'
    if ($originUrl -notmatch $pattern) {
        throw "Unrecognized origin remote URL: $originUrl"
    }

    $Repo = "$($Matches.owner)/$($Matches.name)"
}

Write-Host "[INFO] Target repository: $Repo"

$syncedCount = 0
$skippedCount = 0

foreach ($rawLine in Get-Content -Path $SecretsFile -Encoding UTF8) {
    $line = $rawLine.Trim()

    if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith('#')) {
        continue
    }

    $parts = $line -split '=', 2
    if ($parts.Count -ne 2 -or [string]::IsNullOrWhiteSpace($parts[0])) {
        throw "Invalid secrets line: $rawLine"
    }

    $name = $parts[0].Trim()
    $value = $parts[1]

    if ([string]::IsNullOrWhiteSpace($value)) {
        Write-Warning "Skipping empty secret: $name"
        $skippedCount++
        continue
    }

    Write-Host "[INFO] Syncing secret: $name"
    gh secret set $name --repo $Repo --body $value | Out-Null

    if ($LASTEXITCODE -ne 0) {
        throw "Failed to sync secret: $name"
    }

    $syncedCount++
}

Write-Host "[SUCCESS] Synced $syncedCount GitHub Actions secrets to $Repo ($skippedCount skipped)."
