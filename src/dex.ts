// How the processes reach Dex: the server address, TLS, and the API key a remote or Live server exchanges for tokens.
// The Live credential and its CA handling follow the Dex Cloud node-ts base.

import { readFileSync } from "node:fs";
import { request } from "node:https";
import { rootCertificates } from "node:tls";

import { ApiKeyCredential, type Credential } from "@superdurable/dex";

/** Client and Worker options of Dex: local `dexcli dev` by default, or a remote or Live server with an API key. */
export interface DexOptions {
  readonly serverAddress: string;
  readonly flowServiceTls?: object;
  readonly credential?: Credential;
}

/**
 * DEX_FLOW_SERVICE_ADDRESS, DEX_TLS, and the API key exchange (DEX_API_KEY, DEX_TOKEN_URL, DEX_SERVER_ID). Live sets
 * the address and the key itself; on Native it also sets DEX_TOKEN_CA_FILE, which the token exchange trusts besides
 * the bundled roots. Preview passes no key.
 */
export function dexOptions(environment: NodeJS.ProcessEnv): DexOptions {
  const value = (name: string) => environment[name]?.trim() || undefined;
  const apiKey = value("DEX_API_KEY");
  const caFile = value("DEX_TOKEN_CA_FILE");
  const required = (name: string) => value(name) ?? (() => { throw new Error(`${name} is required with DEX_API_KEY`); })();
  return {
    serverAddress: value("DEX_FLOW_SERVICE_ADDRESS") ?? "127.0.0.1:8801",
    ...(["1", "true"].includes(value("DEX_TLS") ?? "") ? { flowServiceTls: {} } : {}),
    ...(apiKey === undefined
      ? {}
      : {
          credential: new ApiKeyCredential({
            tokenUrl: required("DEX_TOKEN_URL"),
            serverId: required("DEX_SERVER_ID"),
            apiKey,
            ...(caFile === undefined ? {} : { fetch: fetchTrusting([...rootCertificates, readFileSync(caFile, "utf8")]) }),
          }),
        }),
  };
}

/** A fetch over node:https that trusts exactly the certificates ca, for the token exchange. */
function fetchTrusting(ca: string[]): typeof globalThis.fetch {
  return async (input, init) => {
    const outgoing = new Request(input, init);
    const body = outgoing.body === null ? undefined : Buffer.from(await outgoing.arrayBuffer());
    return new Promise<Response>((resolve, reject) => {
      const sent = request(outgoing.url, { method: outgoing.method, headers: Object.fromEntries(outgoing.headers), ca, signal: outgoing.signal }, (incoming) => {
        const chunks: Buffer[] = [];
        incoming.on("data", (chunk: Buffer) => chunks.push(chunk));
        incoming.on("error", reject);
        incoming.on("end", () => {
          const headers = new Headers();
          for (const [name, value] of Object.entries(incoming.headers)) {
            for (const item of Array.isArray(value) ? value : value === undefined ? [] : [value]) {
              headers.append(name, item);
            }
          }
          const status = incoming.statusCode ?? 502;
          const empty = [101, 204, 205, 304].includes(status);
          resolve(new Response(empty ? null : Buffer.concat(chunks), { status, headers }));
        });
      });
      sent.on("error", reject);
      sent.end(body);
    });
  };
}
