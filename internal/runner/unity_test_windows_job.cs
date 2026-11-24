namespace LoopForge
{
    using System;
    using System.Runtime.InteropServices;
    using System.Threading.Tasks;
    using Microsoft.Win32.SafeHandles;

    public static class UnityTestJob
    {
        [DllImport("kernel32.dll")]
        private static extern IntPtr GetCurrentProcess();

        [DllImport("kernel32.dll")]
        private static extern bool TerminateProcess(IntPtr process, uint exitCode);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern SafeFileHandle CreateFile(string path, uint access, uint shareMode, IntPtr security, uint disposition, uint flags, IntPtr template);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool SetFileInformationByHandle(SafeFileHandle file, int infoClass, ref byte deleteFile, uint size);

        public static void WatchForInterruption(Task<int> canceled, int timeoutMilliseconds)
        {
            // The launcher owns the kill-on-close job. This thread is independent
            // of the launcher's polling, I/O and termination of the Editor.
            Task.Run(async () => {
                await Task.WhenAny(canceled, Task.Delay(timeoutMilliseconds));
                await Task.Delay(12000);
                TerminateProcess(GetCurrentProcess(), 124);
            });
        }

        public static void RemoveUnusedLock(string path)
        {
            const uint deleteAccess = 0x10000, exclusive = 0, openExisting = 3, openReparsePoint = 0x00200000;
            const int fileDispositionInfo = 4;
            // A running Unity editor holds this file open. Exclusive access
            // also prevents an Editor from acquiring it between open and remove.
            using (var file = CreateFile(path, deleteAccess, exclusive, IntPtr.Zero, openExisting, openReparsePoint, IntPtr.Zero))
            {
                if (file.IsInvalid) return;
                byte deleteFile = 1;
                SetFileInformationByHandle(file, fileDispositionInfo, ref deleteFile, 1);
            }
        }
    }
}
