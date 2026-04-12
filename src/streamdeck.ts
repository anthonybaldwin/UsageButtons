/**
 * Minimal Stream Deck SDK wrapper.
 *
 * The Stream Deck software launches our compiled binary with these
 * command-line arguments:
 *
 *   -port <port> -pluginUUID <uuid> -registerEvent <event> -info <json>
 *
 * We connect to `ws://127.0.0.1:<port>`, send a single registration
 * message, and then exchange JSON events for the lifetime of the app.
 *
 * Protocol reference: https://docs.elgato.com/streamdeck/sdk/references/events-received/
 *
 * We deliberately do not pull in `@elgato/streamdeck` — it assumes a
 * Node.js runtime and we want `bun build --compile` to produce a
 * standalone binary with zero JS dependencies at runtime.
 */

export interface RegistrationArgs {
  port: number;
  pluginUUID: string;
  registerEvent: string;
  info: unknown;
}

export function parseArgs(argv: readonly string[]): RegistrationArgs {
  const get = (flag: string): string => {
    const idx = argv.indexOf(flag);
    if (idx < 0 || idx + 1 >= argv.length) {
      throw new Error(`missing required arg ${flag}`);
    }
    const value = argv[idx + 1];
    if (value === undefined) throw new Error(`missing value for ${flag}`);
    return value;
  };

  const port = Number.parseInt(get("-port"), 10);
  if (!Number.isFinite(port)) throw new Error(`invalid -port`);

  return {
    port,
    pluginUUID: get("-pluginUUID"),
    registerEvent: get("-registerEvent"),
    info: JSON.parse(get("-info")) as unknown,
  };
}

/** Events we emit from plugin → Stream Deck. */
export type OutboundEvent =
  | { event: string; uuid: string } // registration
  | {
      event: "setImage";
      context: string;
      payload: { image: string; target?: 0 | 1 | 2; state?: number };
    }
  | {
      event: "setTitle";
      context: string;
      payload: { title: string; target?: 0 | 1 | 2; state?: number };
    }
  | {
      event: "setSettings";
      context: string;
      payload: Record<string, unknown>;
    }
  | {
      event: "getSettings";
      context: string;
    }
  | {
      event: "getGlobalSettings";
      context: string;
    }
  | {
      event: "setGlobalSettings";
      context: string;
      payload: Record<string, unknown>;
    }
  | {
      event: "logMessage";
      payload: { message: string };
    }
  | {
      event: "showAlert";
      context: string;
    }
  | {
      event: "showOk";
      context: string;
    }
  | {
      event: "openUrl";
      payload: { url: string };
    };

/** Events we receive from Stream Deck → plugin. */
export interface InboundEventBase {
  event: string;
  action?: string;
  context?: string;
  device?: string;
}

export interface WillAppearEvent extends InboundEventBase {
  event: "willAppear";
  action: string;
  context: string;
  device: string;
  payload: {
    settings: Record<string, unknown>;
    coordinates: { column: number; row: number };
    state?: number;
    isInMultiAction?: boolean;
  };
}

export interface WillDisappearEvent extends InboundEventBase {
  event: "willDisappear";
  action: string;
  context: string;
  device: string;
  payload: {
    settings: Record<string, unknown>;
    coordinates: { column: number; row: number };
    state?: number;
    isInMultiAction?: boolean;
  };
}

export interface KeyDownEvent extends InboundEventBase {
  event: "keyDown";
  action: string;
  context: string;
  device: string;
  payload: {
    settings: Record<string, unknown>;
    coordinates: { column: number; row: number };
    state?: number;
    userDesiredState?: number;
    isInMultiAction?: boolean;
  };
}

export interface DidReceiveSettingsEvent extends InboundEventBase {
  event: "didReceiveSettings";
  action: string;
  context: string;
  device: string;
  payload: {
    settings: Record<string, unknown>;
    coordinates: { column: number; row: number };
    isInMultiAction?: boolean;
  };
}

export type InboundEvent =
  | WillAppearEvent
  | WillDisappearEvent
  | KeyDownEvent
  | DidReceiveSettingsEvent
  | InboundEventBase;

/**
 * Thin wrapper around the registration websocket. Owns the socket
 * lifecycle and exposes typed send/receive helpers.
 */
export class StreamDeckConnection {
  private ws: WebSocket | null = null;
  private readonly handlers = new Set<(event: InboundEvent) => void>();
  private readonly openHandlers = new Set<() => void>();

  constructor(private readonly args: RegistrationArgs) {}

  async connect(): Promise<void> {
    const url = `ws://127.0.0.1:${this.args.port}`;
    this.log(`connecting to ${url}`);
    const ws = new WebSocket(url);
    this.ws = ws;

    return new Promise((resolve, reject) => {
      ws.addEventListener("open", () => {
        this.send({
          event: this.args.registerEvent,
          uuid: this.args.pluginUUID,
        });
        this.log("registered");
        for (const h of this.openHandlers) h();
        resolve();
      });
      ws.addEventListener("error", (ev) => {
        reject(new Error(`websocket error: ${String(ev)}`));
      });
      ws.addEventListener("message", (ev) => {
        let parsed: InboundEvent;
        try {
          parsed = JSON.parse(String(ev.data)) as InboundEvent;
        } catch (err) {
          this.log(`failed to parse inbound: ${String(err)}`);
          return;
        }
        for (const h of this.handlers) h(parsed);
      });
      ws.addEventListener("close", () => {
        this.log("socket closed");
      });
    });
  }

  onEvent(handler: (event: InboundEvent) => void): void {
    this.handlers.add(handler);
  }

  onOpen(handler: () => void): void {
    this.openHandlers.add(handler);
  }

  send(event: OutboundEvent): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      // Can't call this.log() here — log() in turn calls send() to
      // emit a logMessage event, which would recurse forever when the
      // socket isn't open yet. Write straight to stderr instead.
      // eslint-disable-next-line no-console
      console.error(
        `[UsageButtons] drop outbound (socket not open): ${event.event}`,
      );
      return;
    }
    this.ws.send(JSON.stringify(event));
  }

  setImage(context: string, svg: string): void {
    const dataUri = `data:image/svg+xml;base64,${Buffer.from(svg, "utf8").toString("base64")}`;
    this.send({
      event: "setImage",
      context,
      payload: { image: dataUri, target: 0 },
    });
  }

  setTitle(context: string, title: string): void {
    this.send({
      event: "setTitle",
      context,
      payload: { title, target: 0 },
    });
  }

  openUrl(url: string): void {
    this.send({ event: "openUrl", payload: { url } });
  }

  log(message: string): void {
    // Stream Deck writes logMessage payloads to its own log file when
    // the socket is open; the send() guard downgrades to stderr when
    // it's not, so we can call this safely during startup too.
    this.send({ event: "logMessage", payload: { message } });
    // eslint-disable-next-line no-console
    console.error(`[UsageButtons] ${message}`);
  }
}
