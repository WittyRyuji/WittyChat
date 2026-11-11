param([string]$ExpectedTag = "")

$ErrorActionPreference = "Stop"
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root
$version = (Get-Content "$root/VERSION" -Raw).Trim()
if ($version -notmatch '^\d+\.\d+\.\d+-alpha(?:\.\d+)?$') {
    throw "VERSION must identify an alpha prerelease"
}
if ($ExpectedTag -and $ExpectedTag -ne "v$version") {
    throw "Tag $ExpectedTag does not match VERSION $version"
}
$name = "WittyChat-$version-windows-amd64"
$out = Join-Path $root "release"
$stage = Join-Path $out "_staging/$name"
if (Test-Path $stage) { throw "Refusing to overwrite existing release directory: $stage" }
New-Item -ItemType Directory -Path $stage -Force | Out-Null

# Build from generated frontend assets, never from a running development exe.
# 从生成的前端资源构建，不复制本机运行中的开发二进制。
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags="-s -w" -o "$stage/wittychat-server.exe" ./server
if ($LASTEXITCODE -ne 0) { throw "Server build failed" }
go build -trimpath -ldflags="-s -w" -o "$stage/wittychat-worker.exe" ./worker
if ($LASTEXITCODE -ne 0) { throw "Worker build failed" }
Push-Location "$root/desktop"
try {
    go build -tags production -trimpath -ldflags="-s -w -H windowsgui" -o "$stage/wittychat.exe" .
    if ($LASTEXITCODE -ne 0) { throw "Desktop build failed" }
} finally { Pop-Location }

$documents = @("README.md", "README.en.md", "LICENSE", "VERSION", "CHANGELOG.md",
    "CONTRIBUTING.md", "CLA.md", "GOVERNANCE.md", "SECURITY.md",
    "CODE_OF_CONDUCT.md", "THIRD_PARTY_NOTICES.md")
foreach ($file in $documents) { Copy-Item "$root/$file" $stage }
New-Item -ItemType Directory -Path "$stage/docs", "$stage/deploy", "$stage/third-party" | Out-Null
Copy-Item "$root/docs/GETTING_STARTED.md", "$root/docs/RELEASING.md", "$root/docs/RELEASE_NOTES_$version.md", "$root/docs/DECISIONS.md" "$stage/docs"
Copy-Item "$root/deploy/OPERATIONS.md" "$stage/deploy"
Copy-Item "$root/desktop/frontend/Inter Font License.txt" "$stage/third-party/Inter-OFL.txt"

function Copy-Notices([string]$Directory, [string]$Destination) {
    $files = @(Get-ChildItem -LiteralPath $Directory -File | Where-Object {
        $_.Name -match '^(LICENSE|LICENCE|COPYING|NOTICE|AUTHORS)([._-].*)?$' -and
        $_.Extension -in @("", ".txt", ".md", ".rst", ".html", ".markdown", ".mit", ".apache", ".gpl", ".bsd", ".ofl")
    })
    if ($files.Count -gt 0) {
        New-Item -ItemType Directory -Path $Destination -Force | Out-Null
        foreach ($file in $files) { Copy-Item -LiteralPath $file.FullName $Destination }
    }
}

$modules = @(go list -m -f '{{.Path}}|{{.Dir}}' all)
if ($LASTEXITCODE -ne 0) { throw "Cannot enumerate Go dependencies" }
foreach ($module in $modules) {
    $parts = $module -split '\|', 2
    if ($parts[0] -eq "wittychat" -or !$parts[1]) { continue }
    Copy-Notices $parts[1] "$stage/third-party/go/$($parts[0] -replace '[^A-Za-z0-9._-]', '_')"
}
go list -m -json all | Out-File "$stage/third-party/go-modules.json" -Encoding utf8
if ($LASTEXITCODE -ne 0) { throw "Cannot record Go dependency metadata" }

foreach ($frontend in @("admin", "desktop/frontend")) {
    $store = "$root/$frontend/node_modules/.pnpm"
    if (!(Test-Path $store)) { throw "Install pnpm dependencies first: $frontend" }
    $label = $frontend -replace '/', '-'
    foreach ($entry in Get-ChildItem -LiteralPath $store -Directory) {
        $packages = "$($entry.FullName)/node_modules"
        if (!(Test-Path $packages)) { continue }
        foreach ($package in Get-ChildItem -LiteralPath $packages -Directory) {
            if ($package.LinkType) { continue }
            $dirs = @($package)
            if ($package.Name.StartsWith("@")) {
                $dirs = @(Get-ChildItem -LiteralPath $package.FullName -Directory | Where-Object { !$_.LinkType })
            }
            foreach ($dir in $dirs) {
                $destination = "$stage/third-party/npm-$label/$($entry.Name -replace '[^A-Za-z0-9._-]', '_')"
                Copy-Notices $dir.FullName $destination
                if (Test-Path "$($dir.FullName)/package.json") {
                    New-Item -ItemType Directory -Path $destination -Force | Out-Null
                    Copy-Item "$($dir.FullName)/package.json" $destination
                }
            }
        }
    }
    Copy-Item "$root/$frontend/pnpm-lock.yaml" "$stage/third-party/$label-pnpm-lock.yaml"
}

$revision = git rev-parse HEAD
if ($LASTEXITCODE -ne 0) { throw "Cannot determine source revision" }
@(
    "Version: $version"
    "Source: https://github.com/WittyRyuji/WittyChat/tree/v$version"
    "Commit: $revision"
    "Unsigned Windows amd64 preview; PostgreSQL and WebView2 are not bundled."
    (go version)
    (node --version)
) | Out-File "$stage/BUILD_INFO.txt" -Encoding utf8
$zip = "$out/$name.zip"
if (Test-Path $zip) { throw "Refusing to overwrite existing archive: $zip" }
Compress-Archive -Path "$stage/*" -DestinationPath $zip -CompressionLevel Optimal
$hash = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLowerInvariant()
"$hash  $name.zip" | Out-File "$out/SHA256SUMS.txt" -Encoding ascii
Write-Output "Built $zip"
