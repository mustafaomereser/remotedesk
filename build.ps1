# RemoteDesk derleme betiği. Çıktılar dist/ klasörüne yazılır.
#   RemoteDesk.exe              -> Windows, konsolsuz (normal kullanım)
#   RemoteDesk-console.exe      -> Windows, konsollu (-headless ve hata ayıklama)
#   RemoteDesk-macos-arm64      -> Apple Silicon (M1/M2/M3/M4)
#   RemoteDesk-macos-amd64      -> Intel Mac
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force dist | Out-Null

$env:CGO_ENABLED = '0'

$env:GOOS = 'windows'; $env:GOARCH = 'amd64'
go vet ./...
go build -trimpath -ldflags "-H=windowsgui -s -w" -o dist/RemoteDesk.exe .
go build -trimpath -ldflags "-s -w" -o dist/RemoteDesk-console.exe .
go build -trimpath -o dist/probe.exe ./cmd/probe

$env:GOOS = 'darwin'
foreach ($arch in 'arm64', 'amd64') {
    $env:GOARCH = $arch
    go vet ./...
    go build -trimpath -ldflags "-s -w" -o "dist/RemoteDesk-macos-$arch" .
}

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
Get-ChildItem dist | Select-Object Name, @{n = 'MB'; e = { [math]::Round($_.Length / 1MB, 1) } }
