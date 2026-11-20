param(
    [string]$DatabaseUrl = "postgres://moviepilot:moviepilot@127.0.0.1:55432/moviepilot_test?sslmode=disable",
    [string[]]$Package = @("./internal/database", "./internal/downloadflow", "./internal/organizationflow", "./internal/jobs", "./internal/app"),
    [int]$TimeoutMinutes = 15
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go toolchain was not found"
}

$env:MOVIEPILOT_TEST_DATABASE_URL = $DatabaseUrl
$timeout = "${TimeoutMinutes}m"
go test -tags=integration -timeout $timeout $Package -count=1
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
