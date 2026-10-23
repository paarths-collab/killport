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
python scripts/test-installed-package.py
Write-Output 'PASS Scoop fresh install delivers functional v1.2.0 through its PATH shim'
scoop uninstall killport

# Only this disposable runner's bucket is rewound; its real public remote is unchanged.
$bucket = Join-Path $env:SCOOP 'buckets/paarths-collab'
git -C $bucket fetch origin
git -C $bucket reset --hard ee1062a13d033970d1b4d7aab44cbf6446d0e0aa
scoop install paarths-collab/killport
$before = killport --version | Out-String
if ($before -notmatch '(?<![0-9])v?1\.1\.0(?![0-9])') { throw "Expected installed v1.1.0, got $before" }
Write-Output 'PASS Scoop baseline is the actual v1.1.0 package'
scoop update
scoop update killport
scoop list killport
python scripts/test-installed-package.py
$installed = Get-Content (Join-Path $env:SCOOP 'apps/killport/current/manifest.json') -Raw | ConvertFrom-Json
if ($installed.version -ne '1.2.0') { throw 'Installed Scoop manifest is not v1.2.0' }
Write-Output 'PASS Scoop v1.1.0 -> scoop update -> scoop update killport delivers functional v1.2.0'
