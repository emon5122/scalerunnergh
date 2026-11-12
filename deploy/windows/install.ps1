<#
.SYNOPSIS
  Installs scalerunnergh.exe as a Windows service (native mode: a runner
  process per job, no Docker needed).

.EXAMPLE
  .\install.ps1 -Exe C:\ProgramData\scalerunnergh\scalerunnergh.exe `
                -Config C:\ProgramData\scalerunnergh\config.json `
                -User '.\srg' -Password (Read-Host -AsSecureString)

  Run from an elevated PowerShell. The service account needs "Log on as a
  service" and must not be an administrator. Keep the PAT/App key file
  readable by that account only.
#>
param(
    [Parameter(Mandatory = $true)][string]$Exe,
    [Parameter(Mandatory = $true)][string]$Config,
    [string]$User,
    [securestring]$Password,
    [string]$Name = 'scalerunnergh'
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path $Exe)) { throw "not found: $Exe" }
if (-not (Test-Path $Config)) { throw "not found: $Config" }

# Validate the config before touching the service.
& $Exe --config $Config --check
if ($LASTEXITCODE -ne 0) { throw 'config check failed' }

$existing = Get-Service -Name $Name -ErrorAction SilentlyContinue
if ($existing) {
    if ($existing.Status -ne 'Stopped') { Stop-Service -Name $Name }
    & sc.exe delete $Name | Out-Null
    Start-Sleep -Seconds 2
}

$binPath = '"{0}" --config "{1}"' -f $Exe, $Config
$params = @{
    Name           = $Name
    BinaryPathName = $binPath
    DisplayName    = 'ScaleRunnerGH'
    Description    = 'Ephemeral GitHub Actions runners (one per job)'
    StartupType    = 'Automatic'
}
if ($User) {
    if (-not $Password) { throw '-Password is required with -User' }
    $params.Credential = New-Object System.Management.Automation.PSCredential($User, $Password)
}
New-Service @params | Out-Null

# Restart on failure, and give running jobs time to finish on stop.
& sc.exe failure $Name reset= 86400 actions= restart/10000/restart/60000/restart/300000 | Out-Null
Start-Service -Name $Name
Get-Service -Name $Name
