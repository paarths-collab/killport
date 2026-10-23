$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true
$env:SCOOP = Join-Path $env:RUNNER_TEMP 'killport-scoop'
$installer = Join-Path $env:RUNNER_TEMP 'scoop-install.ps1'
Invoke-WebRequest 'https://raw.githubusercontent.com/ScoopInstaller/Install/1e2f334083d609986d8c8bc9e31ae8e87c39fab4/install.ps1' -OutFile $installer
& $installer -RunAsAdmin -ScoopDir $env:SCOOP
$env:PATH = (Join-Path $env:SCOOP 'shims') + [IO.Path]::PathSeparator + $env:PATH
scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket
scoop install paarths-collab/killport
scoop list killport
$installedLicense = Join-Path (scoop prefix killport | Select-Object -Last 1) 'LICENSE'
if ((Get-FileHash $installedLicense).Hash -ne (Get-FileHash LICENSE).Hash) { throw 'Installed license differs from MIT source license' }
python scripts/test-installed-package.py
Write-Output 'PASS Scoop fresh install delivers functional v1.2.1 through its PATH shim'
scoop uninstall killport

# Only this disposable runner's bucket is rewound; its real public remote is unchanged.
$bucket = Join-Path $env:SCOOP 'buckets/paarths-collab'
git -C $bucket fetch origin
git -C $bucket reset --hard 9bf0bc309f0948dc2d0b72cda2479f1a230a76fe
scoop install paarths-collab/killport
$before = killport --version | Out-String
if ($before -notmatch '(?<![0-9])v?1\.2\.0(?![0-9])') { throw "Expected installed v1.2.0, got $before" }
Write-Output 'PASS Scoop baseline is the actual v1.2.0 package'
scoop update
scoop update killport
scoop list killport
$installedLicense = Join-Path (scoop prefix killport | Select-Object -Last 1) 'LICENSE'
if ((Get-FileHash $installedLicense).Hash -ne (Get-FileHash LICENSE).Hash) { throw 'Installed license differs from MIT source license' }
python scripts/test-installed-package.py
Write-Output 'PASS Scoop v1.2.0 -> scoop update -> scoop update killport delivers functional v1.2.1'
