param(
    [string]$SecretsFile = "",
    [string]$Repo = ""
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new()
# 密钥值和 JSON 一律经 stdin 交给 gh：Windows PowerShell 5.1 给原生程序传参时会吞掉参数里的双引号，
# 命令行参数还会出现在进程列表里。stdin 默认按 ASCII 编码，这里改成不带 BOM 的 UTF-8。
$Utf8NoBom = [System.Text.UTF8Encoding]::new($false)
$OutputEncoding = $Utf8NoBom

# 这些键描述单台服务器，只能写在 [env:名字] 节里；SSH_PORT 可省略，工作流默认 22
$ServerKeys = @('SSH_HOST', 'SSH_PORT', 'SSH_USER', 'SSH_PASSWORD', 'DEPLOY_DIR')
$RequiredServerKeys = @('SSH_HOST', 'SSH_USER', 'SSH_PASSWORD', 'DEPLOY_DIR')
# 与工作流 Resolve deploy targets 步骤的校验规则一致
$EnvironmentNamePattern = '^[A-Za-z0-9._-]+$'

function Invoke-Gh {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [string]$InputText
    )

    if ($PSBoundParameters.ContainsKey('InputText')) {
        # 5.1 打开子进程 stdin 时会按 Console.InputEncoding 先写一个 BOM，控制台为 UTF-8 时它会混进密钥值开头。
        # 只在这次调用期间换成无 BOM 编码，结束后恢复，不影响用户的控制台。
        $previousInputEncoding = [Console]::InputEncoding
        [Console]::InputEncoding = $Utf8NoBom
        try {
            $output = $InputText | & gh @Arguments
        }
        finally {
            [Console]::InputEncoding = $previousInputEncoding
        }
    }
    else {
        $output = & gh @Arguments
    }

    # gh 是原生程序，$ErrorActionPreference 管不到它的退出码，必须显式检查
    if ($LASTEXITCODE -ne 0) {
        throw "gh $($Arguments -join ' ') failed with exit code $LASTEXITCODE."
    }
    return $output
}

# 仓库根目录由 git 决定，脚本放在仓库内任何层级都能用
$RepoRoot = git -C $PSScriptRoot rev-parse --show-toplevel
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($RepoRoot)) {
    throw "Not inside a git repository."
}
$RepoRoot = $RepoRoot.Trim()

if ([string]::IsNullOrWhiteSpace($SecretsFile)) {
    $SecretsFile = Join-Path $PSScriptRoot ".env.secrets"
}

if (-not (Test-Path -Path $SecretsFile)) {
    throw "Secrets file not found: $SecretsFile"
}

# 节外的键写入仓库级 secrets；每个 [env:名字] 节写入同名 Environment 的 secrets，节的先后顺序即部署顺序。
# 整个文件解析校验通过后才开始写 GitHub，避免同步到一半才发现格式错误。
$repoSecrets = [ordered]@{}
$environments = [ordered]@{}
$currentSecrets = $repoSecrets
$currentEnvironment = $null
$lineNumber = 0

foreach ($rawLine in Get-Content -Path $SecretsFile -Encoding UTF8) {
    $lineNumber++
    $line = $rawLine.Trim()

    if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith('#')) {
        continue
    }

    if ($line.StartsWith('[')) {
        if ($line -notmatch '^\[env:(?<name>[^\]]*)\]$') {
            throw "Invalid section header on line ${lineNumber}: expected [env:name]."
        }

        $currentEnvironment = $Matches.name.Trim()
        if ($currentEnvironment -notmatch $EnvironmentNamePattern) {
            throw "Invalid environment name '$currentEnvironment' on line ${lineNumber}: only letters, digits, '.', '_' and '-' are allowed."
        }
        if ($environments.Contains($currentEnvironment)) {
            throw "Duplicate section [env:$currentEnvironment] on line ${lineNumber}."
        }

        $currentSecrets = [ordered]@{}
        $environments[$currentEnvironment] = $currentSecrets
        continue
    }

    # 报错只给行号，不回显整行，免得把密钥值打到终端
    $parts = $line -split '=', 2
    if ($parts.Count -ne 2 -or [string]::IsNullOrWhiteSpace($parts[0])) {
        throw "Invalid secrets line ${lineNumber}: expected NAME=value."
    }

    $name = $parts[0].Trim()
    $value = $parts[1]

    if ($null -eq $currentEnvironment -and $ServerKeys -contains $name) {
        throw "$name on line $lineNumber describes a single server and must be placed under an [env:name] section."
    }
    if ($currentSecrets.Contains($name)) {
        throw "Duplicate secret $name on line ${lineNumber}."
    }

    $currentSecrets[$name] = $value
}

if ($environments.Count -eq 0) {
    throw "No [env:name] section found in $SecretsFile. Each deploy server needs its own section; see deploy/.env.secrets.example."
}

foreach ($environmentName in $environments.Keys) {
    $secrets = $environments[$environmentName]
    $missing = @($RequiredServerKeys | Where-Object {
            -not $secrets.Contains($_) -or [string]::IsNullOrWhiteSpace($secrets[$_])
        })
    if ($missing.Count -gt 0) {
        throw "[env:$environmentName] is missing required secrets: $($missing -join ', ')"
    }
}

if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
    throw "gh was not found. Install GitHub CLI first and run gh auth login."
}

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
Write-Host "[INFO] Deploy targets (in order): $($environments.Keys -join ', ')"

# 只允许 mark-v* tag（推送触发）和默认分支（手动触发默认从这里运行）读取环境里的 SSH 密钥
$defaultBranch = "$(Invoke-Gh @('api', "repos/$Repo", '--jq', '.default_branch'))".Trim()
$deploymentRefs = @(
    @{ type = 'tag'; name = 'mark-v*' },
    @{ type = 'branch'; name = $defaultBranch }
)
$existingEnvironments = @(Invoke-Gh @('api', '--paginate', "repos/$Repo/environments", '--jq', '.environments[].name'))

function Initialize-Environment {
    param([string]$Name)

    if ($existingEnvironments -notcontains $Name) {
        Write-Host "[INFO] Creating environment: $Name"
        Invoke-Gh @('api', '-X', 'PUT', "repos/$Repo/environments/$Name",
            '-F', 'deployment_branch_policy[protected_branches]=false',
            '-F', 'deployment_branch_policy[custom_branch_policies]=true') | Out-Null
    }
    else {
        # 已有环境的保护规则可能是手动调过的，不覆盖，只提示
        $environment = (Invoke-Gh @('api', "repos/$Repo/environments/$Name")) -join "`n" | ConvertFrom-Json
        $policy = $environment.deployment_branch_policy
        if ($null -eq $policy -or -not $policy.custom_branch_policies) {
            Write-Warning "Environment $Name accepts deployments from any branch; limit it under Settings > Environments > $Name > Deployment branches and tags."
            return
        }
    }

    $response = (Invoke-Gh @('api', "repos/$Repo/environments/$Name/deployment-branch-policies?per_page=100")) -join "`n" | ConvertFrom-Json
    foreach ($ref in $deploymentRefs) {
        $matched = @($response.branch_policies | Where-Object {
                $type = if ($_.type) { $_.type } else { 'branch' }
                $type -eq $ref.type -and $_.name -eq $ref.name
            })
        if ($matched.Count -eq 0) {
            Write-Host "[INFO] Allowing $($ref.type) '$($ref.name)' to deploy to $Name"
            Invoke-Gh @('api', '-X', 'POST', "repos/$Repo/environments/$Name/deployment-branch-policies",
                '-f', "name=$($ref.name)", '-f', "type=$($ref.type)") | Out-Null
        }
    }
}

$syncedCount = 0
$skippedCount = 0

function Sync-Secrets {
    param(
        [System.Collections.IDictionary]$Secrets,
        [string]$Environment = ""
    )

    $scope = if ($Environment) { "environment $Environment" } else { "repository" }

    foreach ($name in $Secrets.Keys) {
        $value = $Secrets[$name]

        if ([string]::IsNullOrWhiteSpace($value)) {
            Write-Warning "Skipping empty secret: $name ($scope)"
            $script:skippedCount++
            continue
        }

        Write-Host "[INFO] Syncing secret: $name ($scope)"
        $ghArgs = @('secret', 'set', $name, '--repo', $Repo)
        if ($Environment) {
            $ghArgs += @('--env', $Environment)
        }
        Invoke-Gh -Arguments $ghArgs -InputText $value | Out-Null
        $script:syncedCount++
    }
}

Sync-Secrets -Secrets $repoSecrets

foreach ($environmentName in $environments.Keys) {
    Initialize-Environment -Name $environmentName
    Sync-Secrets -Secrets $environments[$environmentName] -Environment $environmentName
}

# 环境名已按规则校验过，不含引号，可以直接拼 JSON
$targetsJson = '[' + (($environments.Keys | ForEach-Object { '"' + $_ + '"' }) -join ',') + ']'
Write-Host "[INFO] Setting variable DEPLOY_TARGETS=$targetsJson"
Invoke-Gh -Arguments @('variable', 'set', 'DEPLOY_TARGETS', '--repo', $Repo) -InputText $targetsJson | Out-Null

# 环境漏配某个 secret 时，仓库级同名 secret 会被静默顶上，把部署发到错误的服务器
$repoSecretNames = @(Invoke-Gh @('api', '--paginate', "repos/$Repo/actions/secrets", '--jq', '.secrets[].name'))
$shadowed = @($ServerKeys | Where-Object { $repoSecretNames -contains $_ })
if ($shadowed.Count -gt 0) {
    Write-Warning "Repository-level secrets $($shadowed -join ', ') still exist and would silently stand in for any environment that lacks them. After a successful deployment from the environments, delete them:"
    foreach ($name in $shadowed) {
        Write-Host "    gh secret delete $name --repo $Repo"
    }
}

Write-Host "[SUCCESS] Synced $syncedCount GitHub Actions secrets to $Repo ($skippedCount skipped); DEPLOY_TARGETS=$targetsJson."
