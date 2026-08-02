param([string]$Version = "0.1.0")

$ErrorActionPreference = "Stop"
$root = Split-Path $PSScriptRoot -Parent
$out = Join-Path $root "release"

Write-Host "[1/4] building UI..."
Push-Location (Join-Path $root "ui")
npm run build | Out-Null
Pop-Location

Write-Host "[2/4] building kernel (UI embedded)..."
Push-Location (Join-Path $root "kernel")
go build -trimpath -ldflags "-s -w" -o aura.exe ./cmd/aura
Pop-Location

Write-Host "[3/4] staging..."
Remove-Item $out -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force "$out\aura" | Out-Null
Copy-Item "$root\kernel\aura.exe" "$out\aura\"
Copy-Item "$root\README.md" "$out\aura\"
Copy-Item "$root\README-ES.md" "$out\aura\"
Copy-Item "$root\ROADMAP.md" "$out\aura\"
Copy-Item "$root\LICENSE.md" "$out\aura\"
Copy-Item "$root\LICENSE-APACHE-2.0.txt" "$out\aura\"
Copy-Item "$root\LICENSE-AGPL-3.0.txt" "$out\aura\"
Copy-Item "$root\spec" "$out\aura\spec" -Recurse
Copy-Item "$root\sdk" "$out\aura\sdk" -Recurse
Copy-Item "$root\skills" "$out\aura\skills" -Recurse
Get-ChildItem "$out\aura" -Recurse -Directory -Filter "__pycache__" |
    Remove-Item -Recurse -Force

Write-Host "[4/4] zipping..."
$zip = Join-Path $out "aura-$Version-windows-amd64.zip"
Compress-Archive -Path "$out\aura" -DestinationPath $zip -Force
$mb = "{0:N1}" -f ((Get-Item $zip).Length / 1MB)
Write-Host "done: $zip ($mb MB)"
Write-Host "to run: unzip, then .\aura\aura.exe up  (see README.md)"
