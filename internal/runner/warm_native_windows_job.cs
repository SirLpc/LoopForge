using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.IO;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;

public static class LoopForgeWarmGit
{
    private static readonly List<SafeFileHandle> pins = new List<SafeFileHandle>();
    private static readonly List<FileStream> configPins = new List<FileStream>();
    private static IntPtr job;

    [StructLayout(LayoutKind.Sequential)]
    private struct BasicLimits
    {
        public long PerProcessTime, PerJobTime;
        public uint Flags;
        public UIntPtr MinimumWorkingSet, MaximumWorkingSet;
        public uint ActiveProcesses;
        public UIntPtr Affinity;
        public uint PriorityClass, SchedulingClass;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct ExtendedLimits
    {
        public BasicLimits Basic;
        public ulong ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes;
        public UIntPtr ProcessMemory, JobMemory, PeakProcessMemory, PeakJobMemory;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct FileInformation
    {
        public uint Attributes, CreationLow, CreationHigh, AccessLow, AccessHigh;
        public uint WriteLow, WriteHigh, Volume, SizeHigh, SizeLow, Links, FileHigh, FileLow;
    }

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr CreateJobObject(IntPtr security, IntPtr name);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetInformationJobObject(IntPtr job, int kind, ref ExtendedLimits info, uint length);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);
    [DllImport("kernel32.dll")]
    private static extern IntPtr GetCurrentProcess();
    [DllImport("kernel32.dll")]
    private static extern bool CloseHandle(IntPtr handle);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern SafeFileHandle CreateFile(string name, uint access, uint sharing, IntPtr security, uint disposition, uint flags, IntPtr template);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetFileInformationByHandle(SafeFileHandle handle, out FileInformation info);

    public static void FenceProcessTree()
    {
        const uint killOnClose = 0x2000;
        const int extendedLimitInformation = 9;
        job = CreateJobObject(IntPtr.Zero, IntPtr.Zero);
        if (job == IntPtr.Zero)
            throw new Win32Exception();
        var limits = new ExtendedLimits();
        limits.Basic.Flags = killOnClose;
        if (!SetInformationJobObject(job, extendedLimitInformation, ref limits, (uint)Marshal.SizeOf(limits)) ||
            !AssignProcessToJobObject(job, GetCurrentProcess()))
        {
            var error = new Win32Exception();
            CloseHandle(job);
            throw error;
        }
        // The helper process owns the handle until exit. Git and its filters
        // inherit the job, including when the helper itself is interrupted.
    }

    public static void PinCheckout(string path)
    {
        var parents = new Stack<string>();
        for (var dir = Path.Combine(Path.GetFullPath(path), ".git"); dir != null && dir != Path.GetPathRoot(dir); dir = Path.GetDirectoryName(dir))
            parents.Push(dir);
        foreach (var dir in parents)
        {
            PinDirectory(dir);
        }
    }

    public static void PinDirectory(string dir)
    {
        const uint listDirectory = 1, shareReadWrite = 3, openExisting = 3;
        const uint backupSemantics = 0x02000000, openReparsePoint = 0x00200000;
        // Hold the directory open without FILE_SHARE_DELETE while Git runs.
        var handle = CreateFile(dir, listDirectory, shareReadWrite, IntPtr.Zero, openExisting, backupSemantics | openReparsePoint, IntPtr.Zero);
        FileInformation info;
        if (handle.IsInvalid || !GetFileInformationByHandle(handle, out info))
        {
            var error = new Win32Exception();
            handle.Dispose();
            throw error;
        }
        var attributes = (FileAttributes)info.Attributes;
        if ((attributes & (FileAttributes.Directory | FileAttributes.ReparsePoint)) != FileAttributes.Directory)
        {
            handle.Dispose();
            throw new IOException("Windows checkout path contains an indirect directory");
        }
        pins.Add(handle);
    }

    public static void PinConfig(string root)
    {
        foreach (var path in new[] { ".git/config", ".git/config.worktree", ".git/info/attributes", ".git/info/grafts" })
        {
            try
            {
                var name = Path.Combine(root, path);
                if ((File.GetAttributes(name) & (FileAttributes.Directory | FileAttributes.ReparsePoint)) != 0)
                    throw new IOException("Windows Git configuration contains an indirect path");
                var file = File.Open(name, FileMode.Open, FileAccess.Read, FileShare.Read);
                configPins.Add(file);
            }
            catch (FileNotFoundException) { if (path == ".git/config") throw; }
            catch (DirectoryNotFoundException) { if (path == ".git/config") throw; }
        }
    }

    public static void CopyIndex(string root, string destination)
    {
        const uint genericRead = 0x80000000, shareAll = 7, openExisting = 3, openReparsePoint = 0x00200000;
        var path = Path.Combine(root, ".git", "index");
        using (var handle = CreateFile(path, genericRead, shareAll, IntPtr.Zero, openExisting, openReparsePoint, IntPtr.Zero))
        {
            FileInformation info;
            if (handle.IsInvalid || !GetFileInformationByHandle(handle, out info))
                throw new Win32Exception();
            var invalid = FileAttributes.Directory | FileAttributes.ReparsePoint | FileAttributes.Device;
            if (((FileAttributes)info.Attributes & invalid) != 0)
                throw new IOException("Windows Git index is not a regular file");
            using (var input = new FileStream(handle, FileAccess.Read))
            using (var output = new FileStream(destination, FileMode.CreateNew, FileAccess.Write))
                input.CopyTo(output);
            var lastWrite = ((long)info.WriteHigh << 32) | info.WriteLow;
            File.SetLastWriteTimeUtc(destination, DateTime.FromFileTimeUtc(lastWrite));
        }
    }

    public static void PinChangedParents(string root, string[] paths)
    {
        var seen = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        foreach (var path in paths)
        {
            var parent = root;
            var parts = path.Split('/');
            for (var i = 0; i < parts.Length - 1; i++)
            {
                parent = Path.Combine(parent, parts[i]);
                if (seen.Contains(parent))
                    continue;
                try
                {
                    if ((File.GetAttributes(parent) & FileAttributes.Directory) == 0)
                        throw new IOException("Windows Git transition replaces a file with a directory");
                }
                catch (FileNotFoundException) { break; }
                catch (DirectoryNotFoundException) { break; }
                PinDirectory(parent);
                seen.Add(parent);
            }
        }
    }

    public static void CheckCheckoutPaths(string root, string[] changes)
    {
        var checkedPaths = new HashSet<string>(StringComparer.Ordinal);
        var paths = new List<string> {
            ".git/config", ".git/config.worktree", ".git/HEAD", ".git/index",
            ".git/packed-refs", ".git/info/exclude", ".git/objects", ".git/refs", ".git/logs"
        };
        if (changes != null)
            paths.AddRange(changes);
        foreach (var path in paths)
        {
            var current = root;
            foreach (var part in path.Split('/'))
            {
                current = Path.Combine(current, part);
                if (!checkedPaths.Add(current))
                    continue;
                try
                {
                    if ((File.GetAttributes(current) & FileAttributes.ReparsePoint) != 0)
                        throw new IOException("Windows Git path contains an indirect file or directory");
                }
                catch (FileNotFoundException) { break; }
                catch (DirectoryNotFoundException) { break; }
            }
        }
    }
}
