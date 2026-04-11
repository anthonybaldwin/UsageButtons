/**
 * Windows DPAPI decryption helper.
 *
 * Chrome/Edge/Brave on Windows encrypt their cookie-value AES key
 * with the Windows Data Protection API (DPAPI, scope: CurrentUser).
 * The encrypted blob lives inside the browser's `Local State` JSON
 * file as `os_crypt.encrypted_key`, prefixed with ASCII `DPAPI`.
 *
 * To decrypt it we need to call `CryptUnprotectData` bound to the
 * currently logged-in user. Bun on Windows has `bun:ffi` but it's
 * fragile for Windows API work, so we shell out to PowerShell's
 * `[System.Security.Cryptography.ProtectedData]::Unprotect` which
 * wraps the same API and is pre-installed on every supported
 * Windows version. Input + output are base64 so we can round-trip
 * arbitrary byte blobs through process stdio safely.
 *
 * NOTE: Chrome 127+ introduced App-Bound Encryption (ABE) where the
 * encrypted_key starts with `APPB` instead of `DPAPI` and the real
 * key is wrapped by a per-app COM elevation service. We explicitly
 * reject that prefix here; the caller falls back to manual-cookie
 * entry with a clear error so the user knows to paste instead.
 */

export class DpapiError extends Error {
  constructor(message: string, public override readonly cause?: unknown) {
    super(message);
    this.name = "DpapiError";
  }
}

/**
 * Decrypt a DPAPI-protected byte buffer (CurrentUser scope).
 * Input and output are raw bytes; base64 encoding/decoding is used
 * internally only for the PowerShell pipe.
 */
export async function dpapiDecrypt(encrypted: Uint8Array): Promise<Uint8Array> {
  if (process.platform !== "win32") {
    throw new DpapiError("DPAPI decryption is only available on Windows");
  }

  const b64 = Buffer.from(encrypted).toString("base64");
  // The PowerShell one-liner: decode input base64, Unprotect with
  // CurrentUser scope, re-encode as base64 to stdout. `-OutputFormat
  // Text` + a bare `[Convert]::ToBase64String(...)` keeps the output
  // to a single line we can trim + parse without worrying about
  // console width wrapping.
  const script = `[Convert]::ToBase64String([System.Security.Cryptography.ProtectedData]::Unprotect([Convert]::FromBase64String('${b64}'), $null, [System.Security.Cryptography.DataProtectionScope]::CurrentUser))`;

  const result = Bun.spawnSync({
    cmd: [
      "powershell",
      "-NoProfile",
      "-NonInteractive",
      "-ExecutionPolicy",
      "Bypass",
      "-OutputFormat",
      "Text",
      "-Command",
      script,
    ],
    stdout: "pipe",
    stderr: "pipe",
  });

  if (result.exitCode !== 0) {
    const err = result.stderr.toString().trim() || "unknown error";
    throw new DpapiError(`PowerShell DPAPI unprotect failed: ${err}`);
  }

  const out = result.stdout.toString().trim();
  if (!out) throw new DpapiError("PowerShell DPAPI unprotect returned empty output");

  try {
    return new Uint8Array(Buffer.from(out, "base64"));
  } catch (err) {
    throw new DpapiError("failed to decode DPAPI output", err);
  }
}

/**
 * Helper: given the raw bytes of `os_crypt.encrypted_key` (AFTER
 * base64-decoding the JSON string), strip the `DPAPI` ASCII prefix
 * and return the bytes that go to `dpapiDecrypt`. Throws on the
 * `APPB` (Chrome 127+ App-Bound Encryption) prefix which we don't
 * yet support.
 */
export function stripChromiumKeyPrefix(keyBlob: Uint8Array): Uint8Array {
  if (keyBlob.length < 5) {
    throw new DpapiError("os_crypt.encrypted_key blob too short");
  }
  const prefix = Buffer.from(keyBlob.slice(0, 5)).toString("ascii");
  if (prefix === "DPAPI") {
    return keyBlob.slice(5);
  }
  if (prefix === "APPB\0" || Buffer.from(keyBlob.slice(0, 4)).toString("ascii") === "APPB") {
    throw new DpapiError(
      "Chrome 127+ App-Bound Encryption is not supported yet — paste your cookie manually instead.",
    );
  }
  throw new DpapiError(
    `unknown os_crypt.encrypted_key prefix: ${JSON.stringify(prefix)}`,
  );
}
