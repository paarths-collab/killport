#requires -Version 5.1
<#
Run native Windows validation against prebuilt executables. No Go install is
needed. TestBinary requires the repository's testdata/ beside it. The script
creates only disposable child processes; it never targets an existing process.

  .\scripts\test-windows.ps1 -Binary .\killport.exe -TestBinary .\killport.test.exe
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Binary,
    [Parameter(Mandatory = $true)][string]$TestBinary,
    [string]$LogDirectory = (Join-Path ([IO.Path]::GetTempPath()) ('killport-windows-' + [guid]::NewGuid().ToString('N')))
)

$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'Native Windows is required; PowerShell on Linux is not Windows validation.' }
$Binary = (Resolve-Path $Binary).Path
$TestBinary = (Resolve-Path $TestBinary).Path
New-Item -ItemType Directory -Force -Path $LogDirectory | Out-Null
$LogDirectory = (Resolve-Path $LogDirectory).Path
$work = Join-Path $LogDirectory 'scratch'
New-Item -ItemType Directory -Path $work | Out-Null
$children = New-Object 'System.Collections.Generic.List[object]'
$summary = New-Object 'System.Collections.Generic.List[string]'
$shell = (Get-Process -Id $PID).Path

function Assert-True($Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Invoke-Native([string]$Executable, [string[]]$Arguments, [string]$WorkingDirectory) {
    $start = New-Object Diagnostics.ProcessStartInfo
    $start.FileName = $Executable
    # Generated arguments are ports, flags, or Windows file paths, none of
    # which contain embedded quotes or end in a directory separator.
    $start.Arguments = ($Arguments | ForEach-Object { '"' + $_.Replace('"', '\"') + '"' }) -join ' '
    $start.WorkingDirectory = $WorkingDirectory
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    # Rebuild the environment rather than leaving inherited Path/PATH aliases.
    # These native socket tests must not discover a user-selected Docker CLI.
    $start.Environment.Clear()
    foreach ($name in @('SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'COMSPEC')) {
        $value = [Environment]::GetEnvironmentVariable($name)
        if ($value) { $start.Environment[$name] = $value }
    }
    $start.Environment['PATH'] = Join-Path $env:SystemRoot 'System32'
    $start.Environment['PATHEXT'] = '.COM;.EXE;.BAT;.CMD'
    $process = New-Object Diagnostics.Process
    $process.StartInfo = $start
    [void]$process.Start()
    $stdout = $process.StandardOutput.ReadToEndAsync()
    $stderr = $process.StandardError.ReadToEndAsync()
    if (-not $process.WaitForExit(120000)) {
        $process.Kill()
        $process.WaitForExit()
        throw 'Validation command exceeded its two-minute limit.'
    }
    $result = [pscustomobject]@{ Code = $process.ExitCode; Out = $stdout.Result; Err = $stderr.Result }
    $process.Dispose()
    return $result
}

function Invoke-Killport([string[]]$Arguments, [int]$Expected = 0) {
    $result = Invoke-Native -Executable $Binary -Arguments ($Arguments + @('--json')) -WorkingDirectory $work
    Assert-True ($result.Code -eq $Expected) "killport exited $($result.Code), expected ${Expected}: $($result.Err) $($result.Out)"
    return ($result.Out | ConvertFrom-Json)
}

function Pass([string]$Message) {
    $line = 'PASS ' + $Message
    $summary.Add($line)
    Write-Output $line
}

$helper = Join-Path $work 'listener.ps1'
@'
param([string]$Ready, [string]$Control, [int]$Port = 0, [string]$Protocol = 'tcp')
$ErrorActionPreference = 'Stop'
if ($Protocol -eq 'udp') {
    $socket = New-Object Net.Sockets.UdpClient -ArgumentList ([Net.IPEndPoint]::new([Net.IPAddress]::Loopback, $Port))
    $boundPort = $socket.Client.LocalEndPoint.Port
} else {
    $socket = New-Object Net.Sockets.TcpListener -ArgumentList ([Net.IPAddress]::Loopback, $Port)
    $socket.Start()
    $boundPort = $socket.LocalEndpoint.Port
}
[IO.File]::WriteAllText($Ready, [string]$boundPort)
try {
    while ($true) {
        if (Test-Path $Control) {
            if ($Protocol -eq 'udp') { $socket.Close() } else { $socket.Stop() }
            [IO.File]::WriteAllText(($Control + '.closed'), 'closed')
            while ($true) { Start-Sleep -Milliseconds 100 }
        }
        Start-Sleep -Milliseconds 100
    }
} finally {
    if ($Protocol -eq 'udp') { $socket.Close() } else { $socket.Stop() }
}
'@ | Set-Content -Encoding UTF8 -Path $helper

function Start-Listener([string]$Protocol = 'tcp', [int]$Port = 0) {
    $token = [guid]::NewGuid().ToString('N')
    $ready = Join-Path $work ($token + '.ready')
    $control = Join-Path $work ($token + '.control')
    $arguments = @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $helper,
        '-Ready', $ready, '-Control', $control, '-Port', [string]$Port, '-Protocol', $Protocol)
    $quoted = ($arguments | ForEach-Object { '"' + $_ + '"' }) -join ' '
    $process = Start-Process -FilePath $shell -ArgumentList $quoted -PassThru -WindowStyle Hidden
    $children.Add($process)
    $clock = [Diagnostics.Stopwatch]::StartNew()
    while (-not (Test-Path $ready)) {
        if ($process.HasExited -or $clock.Elapsed.TotalSeconds -gt 15) { throw 'Disposable listener did not start.' }
        Start-Sleep -Milliseconds 50
    }
    return [pscustomobject]@{ Process = $process; Port = [int](Get-Content $ready -Raw); Control = $control }
}

function Assert-OwnedPlan($Plan, $Listener) {
    Assert-True (@($Plan.resources).Count -eq 1) 'Plan must name exactly one disposable process.'
    Assert-True ($Plan.resources[0].pid -eq $Listener.Process.Id) 'Refusing to apply a plan targeting a process not created by this test.'
    Assert-True ($Plan.resources[0].identity_strength -eq 'handle') 'Windows plan did not bind a process handle identity.'
}

try {
    $lookup = Invoke-Native -Executable (Join-Path $env:SystemRoot 'System32/where.exe') -Arguments @('docker') -WorkingDirectory $work
    Assert-True ($lookup.Code -eq 1) "Native test environment must not resolve Docker (exit $($lookup.Code)): $($lookup.Out) $($lookup.Err)"
    $suite = Invoke-Native -Executable $TestBinary -Arguments @('-test.v', '-test.count=1') -WorkingDirectory (Split-Path $TestBinary)
    $suite.Out | Set-Content -Encoding UTF8 -Path (Join-Path $LogDirectory 'go-tests.log')
    $suite.Err | Set-Content -Encoding UTF8 -Path (Join-Path $LogDirectory 'go-tests-stderr.log')
    Assert-True ($suite.Code -eq 0) 'Native Windows Go suite failed; see go-tests.log.'
    Assert-True ($suite.Out -match '--- PASS: TestWindowsProcessIdentityAndTermination') 'Windows handle regression did not pass.'
    Pass 'native Windows suite, including real process-handle identity and termination'

    $null = Invoke-Killport -Arguments @('--check')
    foreach ($protocol in @('tcp', 'udp')) {
        $listener = Start-Listener -Protocol $protocol
        $port = [string]$listener.Port
        $observation = Invoke-Killport -Arguments @('inspect', $port, '--protocol', $protocol, '--mode', 'native')
        Assert-True ($observation.endpoints[0].occupied -and $observation.endpoints[0].owners[0].pid -eq $listener.Process.Id) 'Inspect did not identify the owned listener.'
        $planPath = Join-Path $work ($protocol + '-plan.json')
        $plan = Invoke-Killport -Arguments @('plan', $port, '--protocol', $protocol, '--mode', 'native', '--grace', '2s', '-o', $planPath)
        Assert-OwnedPlan $plan $listener
        $result = Invoke-Killport -Arguments @('apply', $planPath)
        Assert-True ($result.actions_executed -eq 1 -and $result.endpoints[0].status -eq 'free') 'Planned endpoint did not become free.'
        Assert-True ($result.resources[0].verified_gone -and $result.resources[0].forced) 'Windows termination was not verified.'
        Assert-True ($listener.Process.WaitForExit(5000)) 'Disposable listener survived apply.'
        Pass ($protocol.ToUpperInvariant() + ' plan/apply terminates the owned process and verifies the endpoint')
    }

    $original = Start-Listener
    $port = [string]$original.Port
    $rows = @(Invoke-Killport -Arguments @('--check', $port))
    Assert-True (@($rows | Where-Object { $_.pid -eq [string]$original.Process.Id }).Count -eq 1) 'Check missed the owned listener.'
    $preview = @(Invoke-Killport -Arguments @($port, '--dry-run'))
    Assert-True ($preview[0].status -eq 'planned' -and -not $original.Process.HasExited) 'Dry-run mutated the owned listener.'
    Pass 'classic check and dry-run preserve the owned listener'

    $driftPath = Join-Path $work 'drift-plan.json'
    $plan = Invoke-Killport -Arguments @('plan', $port, '--mode', 'native', '--grace', '0s', '-o', $driftPath)
    Assert-OwnedPlan $plan $original
    [IO.File]::WriteAllText($original.Control, 'close')
    $clock = [Diagnostics.Stopwatch]::StartNew()
    while (-not (Test-Path ($original.Control + '.closed'))) {
        Assert-True ($clock.Elapsed.TotalSeconds -lt 10) 'Original listener did not close.'
        Start-Sleep -Milliseconds 50
    }
    $replacement = Start-Listener -Port $original.Port
    $result = Invoke-Killport -Arguments @('apply', $driftPath) -Expected 3
    Assert-True ($result.actions_executed -eq 0 -and $result.endpoints[0].status -eq 'blocked_by_new_owner') 'Owner drift was not rejected.'
    Assert-True (-not $original.Process.HasExited -and -not $replacement.Process.HasExited) 'Owner drift signalled a test process.'
    Pass 'owner drift performs zero actions and both processes survive'

    $rows = @(Invoke-Killport -Arguments @($port))
    Assert-True ($rows[0].killed -and $replacement.Process.WaitForExit(5000)) 'Classic command did not terminate the replacement.'
    Pass 'classic command terminates only the owned replacement process'
    $summary | Set-Content -Encoding UTF8 -Path (Join-Path $LogDirectory 'summary.log')
    Write-Output "All Windows validation checks passed. Logs: $LogDirectory"
} catch {
    $_ | Out-String | Set-Content -Encoding UTF8 -Path (Join-Path $LogDirectory 'failure.log')
    throw
} finally {
    foreach ($child in $children) {
        if (-not $child.HasExited) { $child.Kill(); $child.WaitForExit() }
        $child.Dispose()
    }
    Remove-Item -LiteralPath $work -Recurse -Force
}
