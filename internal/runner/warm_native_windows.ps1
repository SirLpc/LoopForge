$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::InputEncoding = [Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$request = ConvertFrom-Json -InputObject ([Console]::In.ReadToEnd())

function Get-LFSScratch([string]$id) {
    if ($id -cnotmatch '^[A-Z2-7]{26}$') {
        throw 'Invalid temporary LFS directory ID'
    }
    return [IO.Path]::Combine([IO.Path]::GetTempPath(), 'loopforge-warm-lfs-' + $id)
}

if ($request.removeScratch) {
    $scratch = Get-LFSScratch $request.removeScratch
    if ([IO.Directory]::Exists($scratch)) {
        [IO.Directory]::Delete($scratch, $true)
    }
    exit 0
}

if ($request.gitArgs) {
    # Resolve the operator's Git outside the checkout. Git sees only the
    # Runner's private configuration, not the user's or the checkout's.
    [Environment]::CurrentDirectory = $env:SystemRoot
    Set-Location -LiteralPath $env:SystemRoot
    $git = Get-Command git.exe -CommandType Application -ErrorAction Stop | Select-Object -First 1
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = $git.Source
    $start.WorkingDirectory = $env:SystemRoot
    $start.Arguments = $request.gitArgs
    $scratch = $null
    if ($request.lfsScratch) {
        $gitRoot = [IO.Directory]::GetParent([IO.Path]::GetDirectoryName($git.Source)).FullName
        foreach ($dir in @('mingw64\bin', 'mingw32\bin', 'bin')) {
            $lfs = Join-Path $gitRoot "$dir\git-lfs.exe"
            if ([IO.File]::Exists($lfs)) {
                $filter = "'" + $lfs.Replace('\', '/').Replace("'", "'\''") + "' filter-process --skip"
                $scratch = Get-LFSScratch $request.lfsScratch
                if ($scratch.StartsWith($request.worktree.TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)) {
                    throw 'LFS scratch must be outside the workspace'
                }
                $start.Arguments = '-c "lfs.storage=' + $scratch + '" -c "filter.lfs.process=' + $filter + '" ' + $start.Arguments
                break
            }
        }
    }
    $start.UseShellExecute = $false
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    foreach ($key in @($start.EnvironmentVariables.Keys)) {
        if ($key -like 'GIT_*') {
            $start.EnvironmentVariables.Remove($key)
        }
    }
    $start.EnvironmentVariables['GIT_CONFIG_NOSYSTEM'] = '1'
    $start.EnvironmentVariables['GIT_CONFIG_GLOBAL'] = 'NUL'
    $start.EnvironmentVariables['GIT_ATTR_NOSYSTEM'] = '1'
    $start.EnvironmentVariables['GIT_LFS_SKIP_SMUDGE'] = '1'
    $start.EnvironmentVariables['GIT_OPTIONAL_LOCKS'] = '0'
    $start.EnvironmentVariables['GIT_TERMINAL_PROMPT'] = '0'
    $start.EnvironmentVariables['GIT_ALLOW_PROTOCOL'] = ''
    $process = $null
    try {
        if ($scratch) {
            $null = [IO.Directory]::CreateDirectory($scratch)
        }
        $process = [Diagnostics.Process]::Start($start)
        $stderr = $process.StandardError.ReadToEndAsync()
        $process.StandardOutput.BaseStream.CopyTo([Console]::OpenStandardOutput())
        $process.WaitForExit()
        [Console]::Error.Write($stderr.GetAwaiter().GetResult())
        exit $process.ExitCode
    } finally {
        if ($process) {
            $process.Dispose()
        }
        if ($scratch -and [IO.Directory]::Exists($scratch)) {
            [IO.Directory]::Delete($scratch, $true)
        }
    }
}

# Return only byte-for-byte matches. An unreadable file, link, unexpected
# representation or unsupported hash defers to the Runner's full proof.
$verified = [Collections.Generic.List[string]]::new()
$parents = [Collections.Generic.Dictionary[string,bool]]::new([StringComparer]::Ordinal)
$buffer = [byte[]]::new(128 * 1024)
$lfsPrefix = "version https://git-lfs.github.com/spec/v1`n"
$ordinaryFile = [IO.FileAttributes]::Directory -bor [IO.FileAttributes]::ReparsePoint -bor [IO.FileAttributes]::Device

function Test-PlainParents([string]$path) {
    $parent = [IO.Path]::GetDirectoryName($path)
    if ($parent -eq $request.worktree) {
        return $true
    }
    if (!$parents.ContainsKey($parent)) {
        $attributes = [IO.File]::GetAttributes($parent)
        $parents[$parent] = ($attributes -band [IO.FileAttributes]::Directory) -and
            !($attributes -band [IO.FileAttributes]::ReparsePoint) -and (Test-PlainParents $parent)
    }
    return $parents[$parent]
}

foreach ($file in $request.files) {
    try {
        $path = [IO.Path]::Combine($request.worktree, $file.path.Replace('/', '\'))
        if (!(Test-PlainParents $path) -or ([IO.File]::GetAttributes($path) -band $ordinaryFile)) {
            continue
        }
        $stream = [IO.File]::Open($path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        try {
            $length = $stream.Length
            $oid = $file.blob
            $algorithm = [Security.Cryptography.HashAlgorithmName]::SHA1
            if ($file.lfs) {
                if ($length -ne [long]$file.size) { continue }
                $oid = $file.lfs
            }
            if ($oid.Length -eq 64) {
                $algorithm = [Security.Cryptography.HashAlgorithmName]::SHA256
            }
            $hash = [Security.Cryptography.IncrementalHash]::CreateHash($algorithm)
            try {
                if (!$file.lfs) {
                    $hash.AppendData([Text.Encoding]::ASCII.GetBytes("blob $length`0"))
                }
                $count = $stream.Read($buffer, 0, $buffer.Length)
                if (!$file.lfs -and $length -ge $lfsPrefix.Length -and $length -le 1024 -and
                    ($count -lt $lfsPrefix.Length -or [Text.Encoding]::ASCII.GetString($buffer, 0, $lfsPrefix.Length) -ceq $lfsPrefix)) {
                    continue
                }
                while ($count -gt 0) {
                    $hash.AppendData($buffer, 0, $count)
                    $count = $stream.Read($buffer, 0, $buffer.Length)
                }
                $digest = [BitConverter]::ToString($hash.GetHashAndReset()).Replace('-', '')
                if ($stream.Position -eq $length -and $digest -ieq $oid) {
                    $verified.Add($file.path)
                }
            } finally {
                $hash.Dispose()
            }
        } finally {
            $stream.Dispose()
        }
    } catch [IO.IOException], [UnauthorizedAccessException] {
        continue
    }
}
[Console]::Out.Write((ConvertTo-Json -InputObject $verified.ToArray() -Compress))
