# Run before opening Subtrans. Records process names/IDs and visible console
# windows, without command lines, window titles, file paths, or subtitle text.
param([ValidateRange(1, 300)][int]$DurationSeconds = 45)
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;

public static class DynAppConsoleTrace {
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    struct Entry {
        public uint size, usage, pid;
        public UIntPtr heap;
        public uint module, threads, parent;
        public int priority;
        public uint flags;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 260)] public string name;
    }
    delegate bool WindowCallback(IntPtr window, IntPtr data);
    [DllImport("kernel32.dll")] static extern IntPtr CreateToolhelp32Snapshot(uint flags, uint pid);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)] static extern bool Process32FirstW(IntPtr snapshot, ref Entry entry);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)] static extern bool Process32NextW(IntPtr snapshot, ref Entry entry);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
    [DllImport("user32.dll")] static extern bool EnumWindows(WindowCallback callback, IntPtr data);
    [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr window);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetClassNameW(IntPtr window, StringBuilder name, int count);
    [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr window, out uint pid);

    static Dictionary<uint, Entry> Processes() {
        var result = new Dictionary<uint, Entry>();
        IntPtr handle = CreateToolhelp32Snapshot(2, 0);
        if (handle == new IntPtr(-1)) return result;
        try {
            var entry = new Entry { size = (uint)Marshal.SizeOf(typeof(Entry)) };
            if (Process32FirstW(handle, ref entry)) do { result[entry.pid] = entry; } while (Process32NextW(handle, ref entry));
        } finally { CloseHandle(handle); }
        return result;
    }
    static string Chain(uint pid, Dictionary<uint, Entry> processes) {
        var names = new List<string>();
        var seen = new HashSet<uint>();
        for (int depth = 0; depth < 6 && pid != 0 && seen.Add(pid); depth++) {
            Entry entry;
            if (!processes.TryGetValue(pid, out entry)) { names.Add("exited[" + pid + "]"); break; }
            names.Add(entry.name + "[" + pid + "]");
            pid = entry.parent;
        }
        return String.Join(" <- ", names);
    }
    static Dictionary<IntPtr, string> Windows() {
        var result = new Dictionary<IntPtr, string>();
        EnumWindows((window, data) => {
            if (!IsWindowVisible(window)) return true;
            var name = new StringBuilder(256);
            GetClassNameW(window, name, name.Capacity);
            string kind = name.ToString();
            if (kind == "ConsoleWindowClass" || kind == "CASCADIA_HOSTING_WINDOW_CLASS") result[window] = kind;
            return true;
        }, IntPtr.Zero);
        return result;
    }
    static bool Relevant(string name) {
        name = name.ToLowerInvariant();
        return name == "cmd.exe" || name == "conhost.exe" || name == "windowsterminal.exe" || name == "powershell.exe" || name == "pwsh.exe" || name == "node.exe" || name.StartsWith("codex") || name.StartsWith("claude") || name.StartsWith("ffmpeg") || name.StartsWith("dynapp-shell-agent");
    }
    public static string[] Record(int seconds) {
        var lines = new List<string>();
        var processes = Processes();
        var seenProcesses = new HashSet<uint>(processes.Keys);
        var visible = Windows();
        var timer = Stopwatch.StartNew();
        long nextSnapshot = 0;
        while (timer.Elapsed.TotalSeconds < seconds) {
            var current = Windows();
            foreach (var window in current) if (!visible.ContainsKey(window.Key)) {
                uint pid;
                GetWindowThreadProcessId(window.Key, out pid);
                var live = Processes();
                foreach (var entry in live) processes[entry.Key] = entry.Value;
                lines.Add(DateTime.Now.ToString("HH:mm:ss.fff") + " WINDOW " + window.Value + " " + Chain(pid, processes));
            }
            visible = current;
            if (timer.ElapsedMilliseconds >= nextSnapshot) {
                var live = Processes();
                foreach (var entry in live) processes[entry.Key] = entry.Value;
                foreach (var entry in live) if (seenProcesses.Add(entry.Key) && Relevant(entry.Value.name))
                    lines.Add(DateTime.Now.ToString("HH:mm:ss.fff") + " PROCESS " + Chain(entry.Key, processes));
                nextSnapshot = timer.ElapsedMilliseconds + 50;
            }
            Thread.Sleep(10);
        }
        if (lines.Count == 0) lines.Add("No console windows or matching process launches were observed.");
        return lines.ToArray();
    }
}
'@
Write-Host "Recording for $DurationSeconds seconds. Open Subtrans and reproduce the flicker now."
[DynAppConsoleTrace]::Record($DurationSeconds)
