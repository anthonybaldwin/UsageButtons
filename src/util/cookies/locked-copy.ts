/**
 * Windows locked-file copy helper.
 *
 * Chrome/Edge/Brave hold their Cookies SQLite DB open with varying
 * degrees of exclusivity while the browser is running:
 *
 *   - Chrome (Google):   exclusive (FileShare.None). No non-admin
 *                        process can read the file — not even
 *                        Microsoft's own `esentutl /y` or .NET's
 *                        FileStream with FileShare.ReadWrite. The
 *                        only bypass is (a) close Chrome briefly,
 *                        (b) use Chrome's own debugging protocol,
 *                        or (c) install a Chrome extension that
 *                        reads cookies via `chrome.cookies.getAll`.
 *                        Verified 2026-04 on Chrome stable.
 *   - Edge (Microsoft):  shareable. `esentutl /y` successfully
 *                        copies the live DB while Edge is running.
 *   - Firefox:           shareable. Plain `fs.copyFileSync` works.
 *
 * We use `esentutl.exe /y SRC /d DST` — a Windows built-in tool
 * originally designed for backing up Jet / ESENT databases (Active
 * Directory, Exchange). It respects whatever sharing mode the
 * holder set and works on arbitrary files including SQLite DBs.
 * This is preferred over a PowerShell `[File]::Open(...,
 * 'ReadWrite,Delete')` trick because esentutl also handles the
 * correct block-by-block read pattern that Windows expects for
 * files held by other processes.
 *
 * For Chrome specifically this function still throws — nothing
 * works for Chrome while it's running. Callers should catch the
 * failure and surface a user-facing hint pointing at Edge,
 * Firefox, or the manual-paste fallback in the Property Inspector.
 */

export class LockedCopyError extends Error {
  constructor(message: string, public override readonly cause?: unknown) {
    super(message);
    this.name = "LockedCopyError";
  }
}

/**
 * Copy `src` to `dst`, tolerating the case where another process has
 * `src` open with exclusive access (Chrome's live Cookies DB, Edge,
 * Brave, etc.). Blocking — uses Bun.spawnSync.
 */
export function copyLockedFileWindows(src: string, dst: string): void {
  if (process.platform !== "win32") {
    throw new LockedCopyError("copyLockedFileWindows only runs on Windows");
  }

  // esentutl /y <source> /d <destination>
  // The /y (copy) mode reads a locked database file using ESENT's
  // backup machinery, which is compatible with SQLite's and
  // Edge/Firefox's sharing modes. Chrome's FileShare.None prevents
  // even this from working, and the caller is expected to catch
  // that case and fall through to a different source.
  //
  // Bun.spawnSync passes argv as a proper vector so we sidestep
  // the Git Bash MSYS2 path-conversion that would otherwise
  // rewrite `/y` and `/d` into Windows paths.
  const result = Bun.spawnSync({
    cmd: ["esentutl.exe", "/y", src, "/d", dst],
    stdout: "pipe",
    stderr: "pipe",
  });

  if (result.exitCode !== 0) {
    const stdout = result.stdout.toString();
    const stderr = result.stderr.toString();
    // esentutl writes its own error messages to stdout (not stderr)
    // so we have to grep both for the actual failure reason.
    const detail = (stderr.trim() || stdout.trim()).replace(/\s+/g, " ");
    throw new LockedCopyError(
      `esentutl /y failed (${result.exitCode}): ${detail.slice(0, 400)}`,
    );
  }
}
