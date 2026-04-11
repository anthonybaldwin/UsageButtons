/**
 * HTTP helper.
 *
 * Providers call upstream APIs through this wrapper so timeout,
 * error classification, and logging behave consistently. It's a thin
 * layer over the global `fetch` (Bun's fetch is native-fast and
 * compatible enough — no `node-fetch` / `undici` dependency).
 *
 * Error classification is deliberately coarse: transport vs. http vs.
 * parse. Provider code then decides what a given status means in its
 * own context (e.g., 401 for Claude = token expired + refresh
 * needed; 401 for OpenRouter = wrong API key + user fix needed).
 */

export interface HttpRequestOptions {
  url: string;
  method?: "GET" | "POST" | "PATCH" | "PUT" | "DELETE";
  headers?: Record<string, string>;
  /** JSON-serializable body; set when you want an automatic JSON POST. */
  json?: unknown;
  /** Raw body for non-JSON posts. */
  body?: string;
  /** Timeout in milliseconds. Default 15_000. */
  timeoutMs?: number;
  /** Optional abort signal to chain with. */
  signal?: AbortSignal;
}

export class HttpError extends Error {
  constructor(
    public readonly status: number,
    public readonly statusText: string,
    public readonly body: string,
    public readonly url: string,
  ) {
    super(`HTTP ${status} ${statusText} from ${url}: ${truncate(body, 500)}`);
    this.name = "HttpError";
  }
}

export class HttpTransportError extends Error {
  constructor(public readonly url: string, cause: unknown) {
    super(`transport error calling ${url}: ${String(cause)}`);
    this.name = "HttpTransportError";
  }
}

function truncate(s: string, n: number): string {
  if (s.length <= n) return s;
  return `${s.slice(0, n)}…`;
}

/**
 * Fetch + parse JSON. Returns the parsed body on 2xx, throws
 * `HttpError` on non-2xx, `HttpTransportError` on network failure.
 */
export async function httpJson<T>(opts: HttpRequestOptions): Promise<T> {
  const headers: Record<string, string> = { ...(opts.headers ?? {}) };
  let body: string | undefined = opts.body;
  if (opts.json !== undefined) {
    body = JSON.stringify(opts.json);
    headers["content-type"] = headers["content-type"] ?? "application/json";
  }
  if (!headers["accept"]) headers["accept"] = "application/json";

  const controller = new AbortController();
  const timeoutMs = opts.timeoutMs ?? 15_000;
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  const outerSignal = opts.signal;
  if (outerSignal) {
    outerSignal.addEventListener("abort", () => controller.abort(), {
      once: true,
    });
  }

  let res: Response;
  try {
    res = await fetch(opts.url, {
      method: opts.method ?? "GET",
      headers,
      ...(body !== undefined ? { body } : {}),
      signal: controller.signal,
    });
  } catch (err) {
    throw new HttpTransportError(opts.url, err);
  } finally {
    clearTimeout(timeout);
  }

  const text = await res.text();
  if (!res.ok) {
    throw new HttpError(res.status, res.statusText, text, opts.url);
  }
  try {
    return JSON.parse(text) as T;
  } catch (err) {
    throw new HttpTransportError(opts.url, `invalid JSON: ${String(err)}`);
  }
}
