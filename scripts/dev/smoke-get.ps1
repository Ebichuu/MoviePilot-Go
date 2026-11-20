<#
.SYNOPSIS
    对本地运行中的 MoviePilot Go 服务做一次全量 GET 冒烟扫描。

.DESCRIPTION
    从 internal/httpapi/*.go 里抽出所有 GET 路由字面量，用样例参数替换占位符后逐个请求，
    统计状态码分布；出现任何 5xx 即以非零退出码结束，可直接接到 CI 或改完大改之后的回归流程里。

    判定口径（与原项目 v2.15.6 的路由清单一致）：
      200 正常
      400 缺必填参数（预期）
      403 API_TOKEN 别名路由 / 需要资源签名 token 的流式接口（预期）
      404 路径带 /api/v1 前缀却不属于该前缀，或配置项非公开（预期）
      502 依赖的外部服务不可达（如 Emby）
      5xx（除 502）视为缺陷

.PARAMETER BaseUrl
    服务地址，默认 http://localhost:3000

.PARAMETER Username
    管理员用户名，默认 admin

.PARAMETER Password
    管理员密码，默认 moviepilot-dev

.PARAMETER TimeoutSeconds
    单条请求超时，默认 20 秒（Emby 客户端自身超时是 15 秒，低于它会误报成「无法连接」）

.EXAMPLE
    .\scripts\dev\smoke-get.ps1

.EXAMPLE
    .\scripts\dev\smoke-get.ps1 -BaseUrl http://localhost:3001 -TimeoutSeconds 20
#>
[CmdletBinding()]
param(
    [string]$BaseUrl = 'http://localhost:3000',
    [string]$Username = 'admin',
    [string]$Password = 'moviepilot-dev',
    [int]$TimeoutSeconds = 20
)

$ErrorActionPreference = 'Stop'
$repoRoot = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$httpapiDir = Join-Path $repoRoot 'internal\httpapi'

if (-not (Test-Path $httpapiDir)) {
    Write-Error "找不到 $httpapiDir，请在仓库内运行本脚本"
    exit 2
}

Write-Host "登录 $BaseUrl ..."
$login = Invoke-RestMethod -Uri "$BaseUrl/api/v1/login/access-token" -Method Post `
    -Body @{ username = $Username; password = $Password } `
    -ContentType 'application/x-www-form-urlencoded' -TimeoutSec 20
$headers = @{ Authorization = "Bearer $($login.access_token)" }

# 占位符 → 样例值。新增路由若带新占位符，在这里补一行即可。
$samples = @{
    '{username}' = 'admin'; '{user_id}' = '1'; '{user_name}' = 'admin'; '{id}' = '1'
    '{subscribe_id}' = '1'; '{share_id}' = '1'; '{episode_id}' = '1'
    '{tmdbid}' = '1399'; '{doubanid}' = '1292052'; '{bangumiid}' = '132413'
    '{anilist_id}' = '16498'; '{person_id}' = '287'; '{season}' = '1'
    '{episode_group}' = '5e9077d2e640d600151f32bd'; '{collection_id}' = '726871'
    '{storage}' = 'local'; '{key}' = 'Sites'; '{plugin_id}' = 'TestPlugin'
    '{mediaid}' = 'themoviedb:1399'; '{type_name}' = '电影'; '{keyword}' = 'Dune'
    '{path}' = 'x'; '{token}' = 'x'; '{cache_key}' = 'x'; '{session_id}' = '1'
    '{file_id}' = '1'; '{download_id}' = '1'; '{hash}' = 'x'
}

$routes = @()
foreach ($file in Get-ChildItem $httpapiDir -Filter *.go | Where-Object { $_.Name -notmatch '_test\.go$' }) {
    $text = Get-Content $file.FullName -Raw
    foreach ($match in [regex]::Matches($text, '\.Get\("(/[^"]+)"')) {
        $routes += $match.Groups[1].Value
    }
}
$routes = $routes | Sort-Object -Unique

# 破坏性 GET（V2 原样保留的「重置」类动作）：扫描一次就会把真实配置清空，
# 必须排除在冒烟之外，否则脚本会毁掉正在验证的站点/订阅数据。
$destructiveRoutes = @('/site/reset')
$skippedDestructive = @($routes | Where-Object { $destructiveRoutes -contains $_ })
$routes = @($routes | Where-Object { $destructiveRoutes -notcontains $_ })

Write-Host "发现 GET 路由 $($routes.Count) 条，开始扫描 ..."
if ($skippedDestructive.Count -gt 0) {
    Write-Host ("跳过破坏性端点 {0} 条：{1}" -f $skippedDestructive.Count, ($skippedDestructive -join ', '))
}

$results = @()
foreach ($route in $routes) {
    $url = $route
    foreach ($key in $samples.Keys) { $url = $url.Replace($key, $samples[$key]) }
    if ($url -match '\{') { continue }   # 还有没配样例值的占位符，跳过并报告

    try {
        $response = Invoke-WebRequest -Uri ($BaseUrl + '/api/v1' + $url) -Headers $headers -TimeoutSec $TimeoutSeconds
        $results += [pscustomobject]@{ Path = $route; Code = [int]$response.StatusCode }
    }
    catch {
        $code = if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }
        $results += [pscustomobject]@{ Path = $route; Code = $code }
    }
}

Write-Host ''
$results | Group-Object Code | Sort-Object { [int]$_.Name } -Descending |
    ForEach-Object { Write-Host ("HTTP {0,-3} {1,4} 条" -f $_.Name, $_.Count) }

$defects = $results | Where-Object { $_.Code -ge 500 -and $_.Code -ne 502 }
if ($defects) {
    Write-Host ''
    Write-Host '发现 5xx（502 除外，属外部依赖不可达）：' -ForegroundColor Red
    $defects | ForEach-Object { Write-Host ("  {0} {1}" -f $_.Code, $_.Path) -ForegroundColor Red }
}

$unrouted = $results | Where-Object { $_.Code -eq 0 }
if ($unrouted) {
    Write-Host ''
    Write-Host '无法建立连接的端点：'
    $unrouted | ForEach-Object { Write-Host ("  0   {0}" -f $_.Path) }
}

Write-Host ''
Write-Host ("扫描完成：共 {0} 条，5xx（含 502）{1} 条，其中内部错误 {2} 条。" -f `
    $results.Count, ($results | Where-Object { $_.Code -ge 500 }).Count, $defects.Count)

if ($defects) { exit 1 }
exit 0
