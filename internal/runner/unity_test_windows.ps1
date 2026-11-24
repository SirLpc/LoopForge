$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::InputEncoding = [Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)

function Read-Report([string]$path, [int]$limit, [bool]$tail) {
    if (![IO.File]::Exists($path)) { return $null }
    $stream = [IO.File]::OpenRead($path)
    try {
        if ($stream.Length -gt $limit) {
            if (!$tail) { throw 'NUnit XML exceeds the Runner report limit' }
            $stream.Position = $stream.Length - $limit
        }
        $reader = [IO.BinaryReader]::new($stream)
        return [Convert]::ToBase64String($reader.ReadBytes([int]($stream.Length - $stream.Position)))
    } finally {
        $stream.Dispose()
    }
}

$result = @{ status = 'infra_error' }
$scratch = $null
$process = $null
try {
    $request = ConvertFrom-Json -InputObject ([Console]::In.ReadLine())
    $project = [IO.Path]::GetFullPath($request.project)
    $editor = [IO.Path]::GetFullPath($request.editor)
    Set-Location -LiteralPath $env:SystemRoot
    Add-Type -TypeDefinition $request.job
    [LoopForgeWarmGit]::FenceProcessTree()
    $parents = [Collections.Generic.Stack[string]]::new()
    for ($dir = [IO.Path]::Combine($project, 'ProjectSettings'); $dir -ne [IO.Path]::GetPathRoot($dir); $dir = [IO.Path]::GetDirectoryName($dir)) {
        $parents.Push($dir)
    }
    foreach ($dir in $parents) { [LoopForgeWarmGit]::PinDirectory($dir) }
    $versionFile = [IO.Path]::Combine($project, 'ProjectSettings', 'ProjectVersion.txt')
    if ([IO.File]::GetAttributes($versionFile) -band [IO.FileAttributes]::ReparsePoint) {
        throw 'Unity ProjectVersion.txt is indirect'
    }
    $version = [regex]::Match([IO.File]::ReadAllText($versionFile), '(?m)^m_EditorVersion:[ \t]*(\S+)').Groups[1].Value
    if (![IO.File]::Exists($editor)) { throw 'configured Unity.exe is unavailable' }
    $actual = ([Diagnostics.FileVersionInfo]::GetVersionInfo($editor).ProductVersion -split '_', 2)[0]
    if (!$version -or $version -cne $actual) {
        throw "Unity Editor version mismatch: project requires $version, configured Editor is $actual"
    }

    [Console]::WriteLine('loopforge-unity-ready')
    if ([Console]::In.ReadLine() -cne 'run') { throw 'Runner did not authorize the Unity launch' }
    $inputPipe = [Console]::OpenStandardInput()
    $cancelled = $inputPipe.ReadAsync([byte[]]::new(1), 0, 1)
    if ($cancelled.IsCompleted) { throw 'Unity test request was canceled before launch' }
    $lock = [IO.Path]::Combine($project, 'Temp', 'UnityLockfile')
    if ([IO.File]::Exists($lock)) {
        $result.status = 'busy'
        throw 'the leased Unity project is open in an editor'
    }
    $scratch = Join-Path ([IO.Path]::GetTempPath()) ('loopforge-unity-' + [Guid]::NewGuid().ToString('N'))
    [void][IO.Directory]::CreateDirectory($scratch)
    $xml = Join-Path $scratch 'results.xml'
    $log = Join-Path $scratch 'Editor.log'

    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = $editor
    $start.WorkingDirectory = $project
    $start.Arguments = $request.args + ' -testResults "' + $xml + '" -logFile "' + $log + '"'
    $start.UseShellExecute = $false
    $start.RedirectStandardInput = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    [LoopForge.UnityTestJob]::WatchForInterruption($cancelled, 30 * 60 * 1000)
    $process = [Diagnostics.Process]::Start($start)
    $process.StandardInput.Close()
    $null = $process.StandardOutput.BaseStream.CopyToAsync([IO.Stream]::Null)
    $null = $process.StandardError.BaseStream.CopyToAsync([IO.Stream]::Null)
    $clock = [Diagnostics.Stopwatch]::StartNew()
    $result.status = 'completed'
    while (!$process.WaitForExit(100)) {
        if ($cancelled.IsCompleted -or $clock.Elapsed.TotalMinutes -ge 30) {
            $result.status = if ($cancelled.IsCompleted) { 'canceled' } else { 'timeout' }
            & (Join-Path $env:SystemRoot 'System32\taskkill.exe') /PID $process.Id /T /F 2>$null | Out-Null
            if (!$process.HasExited) { $process.Kill() }
            if ($process.WaitForExit(5000) -and [IO.File]::Exists($lock)) {
                [LoopForgeWarmGit]::PinDirectory([IO.Path]::GetDirectoryName($lock))
                [LoopForge.UnityTestJob]::RemoveUnusedLock($lock)
            }
            break
        }
    }
    if ($process.HasExited) { $result.exitCode = $process.ExitCode }
    $result.logTruncated = [IO.File]::Exists($log) -and [IO.FileInfo]::new($log).Length -gt 4MB
    $result.log = Read-Report $log (4MB) $true
    $result.xml = Read-Report $xml (8MB) $false
} catch {
    if ($result.status -eq 'completed') { $result.status = 'infra_error' }
    $result.message = $_.Exception.Message
} finally {
    if ($process) { $process.Dispose() }
    if ($scratch -and [IO.Directory]::Exists($scratch)) {
        try { [IO.Directory]::Delete($scratch, $true) } catch { }
    }
}
[Console]::WriteLine(($result | ConvertTo-Json -Compress))
