# 在本地起一套能用的 MoviePilot-Go：postgres + redis + 应用（本地构建）。
#
# 定位：开发 / 自用验证。发布路径不受影响，仍只用 compose.yaml + Actions 产出的 digest。
# 这里给的全部是「开发默认值」，只在环境变量为空时填充，方便直接跑起来。
#
# 用法：
#   .\scripts\dev\up.ps1              # 构建并后台启动
#   .\scripts\dev\up.ps1 -Logs        # 前台启动，直接看日志
#   .\scripts\dev\up.ps1 -Down        # 停止（保留数据卷）
#   .\scripts\dev\up.ps1 -Recreate    # 重建应用容器（改了代码后）
[CmdletBinding()]
param(
    [switch]$Logs,
    [switch]$Down,
    [switch]$Recreate,
    [string]$BootstrapPassword = 'moviepilot-dev'
)

$ErrorActionPreference = 'Stop'

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$compose = @(
    '-f', (Join-Path $root 'compose.yaml'),
    '-f', (Join-Path $root 'compose.dev.yaml')
)

function Set-DevEnv([string]$name, [string]$value) {
    if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($name))) {
        Set-Item -Path "env:$name" -Value $value
    }
}

# compose.yaml 用 `${VAR:?}` 把这些变量定为必填，缺一个连 compose 都解析不了。
Set-DevEnv 'MOVIEPILOT_IMAGE' 'moviepilot-go:dev'
Set-DevEnv 'POSTGRES_PASSWORD' 'moviepilot-dev'
Set-DevEnv 'MOVIEPILOT_JWT_SECRET' 'dev-jwt-secret-3f9c1b7a5e2d4c8f'
Set-DevEnv 'MOVIEPILOT_RESOURCE_SECRET' 'dev-resource-secret-8b1e4d6a2c9f5e73'
Set-DevEnv 'MOVIEPILOT_MASTER_KEY' 'dev-master-key-0123456789abcdef'
Set-DevEnv 'MOVIEPILOT_BOOTSTRAP_PASSWORD' $BootstrapPassword
# 本地联调 API_TOKEN 用；置空则 `*2` 只读接口全部 403。
Set-DevEnv 'MOVIEPILOT_API_TOKEN' 'dev-api-token-5a7c9e1b3d6f'

Push-Location $root
try {
    if ($Down) {
        docker compose @compose down
        return
    }
    if ($Recreate) {
        docker compose @compose up -d --build --force-recreate moviepilot-go
    }
    elseif ($Logs) {
        docker compose @compose up --build
    }
    else {
        docker compose @compose up -d --build
    }

    if (-not $Logs) {
        Write-Host ''
        Write-Host '已启动：' -ForegroundColor Green
        Write-Host '  前端/API  http://localhost:3000'
        Write-Host "  管理员     admin / $BootstrapPassword"
        Write-Host '  跟随日志   docker compose -f compose.yaml -f compose.dev.yaml logs -f moviepilot-go'
        Write-Host '  停止       .\scripts\dev\up.ps1 -Down'
    }
}
finally {
    Pop-Location
}
