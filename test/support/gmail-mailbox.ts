// Gmail as the Gmail connector's send read-back meets it (superdurable/dex-connectors-library#1061), for the tests that
// run the released sendMessage over a fake transport. messages.send replaces the client's Message-ID on the Sent copy
// with Gmail's own and keeps the X-Dex-Message-ID header, so a search by rfc822msgid: never finds the client's value;
// a Sent Mail search lists recent candidates, newest first; and a format=metadata read returns a candidate's headers.

import type { ProviderRequest, ProviderResponse } from "@superdurable/connectors-sdk/providerhttp";

/** One message Gmail accepted. */
export interface GmailSend {
  /** The Message-ID the client sent; Gmail keeps no copy of it. */
  readonly messageId: string;
  /** The X-Dex-Message-ID header the client sent, kept on the Sent copy. */
  readonly dexMessageId: string;
  /** The Message-ID of the Sent copy, Gmail's own. */
  readonly sentMessageId: string;
  readonly to: string;
  readonly subject: string;
  /** The ID Gmail gave the message. */
  readonly gmailId: string;
}

const json = (status: number, body: unknown): ProviderResponse => ({ status, headers: {}, body: new TextEncoder().encode(JSON.stringify(body)) });
const notFound = () => json(404, { error: { code: 404, message: "Requested entity was not found." } });
const headerOf = (raw: string, name: string) => new RegExp(`^${name}: *(.*?)\\r?$`, "mi").exec(raw)?.[1] ?? "";

export const isGmailSend = (request: ProviderRequest) => request.method === "POST" && request.url.includes("/messages/send");
export const isSentMailSearch = (request: ProviderRequest) => request.method === "GET" && new URL(request.url).pathname.endsWith("/users/me/messages");
export const isHeaderRead = (request: ProviderRequest) => {
  const url = new URL(request.url);
  return request.method === "GET" && url.pathname.includes("/users/me/messages/") && url.searchParams.get("format") === "metadata";
};

/** The RFC 5322 message of a messages.send request: its raw field, base64url with or without padding. */
export function rawOf(request: ProviderRequest): string {
  const body = JSON.parse(new TextDecoder().decode(request.body)) as { raw: string };
  return Buffer.from(body.raw, "base64url").toString("utf8");
}

/** One mailbox's Sent Mail, with the requests that read it. */
export class GmailMailbox {
  public readonly sends: GmailSend[] = [];
  public readonly searches: string[] = [];
  public readonly headerReads: string[] = [];

  /** Records `raw` in Sent Mail as Gmail accepts it: under a new ID, with Gmail's own Message-ID. */
  public send(raw: string): GmailSend {
    const number = this.sends.length + 1;
    const send: GmailSend = {
      messageId: headerOf(raw, "Message-ID"),
      dexMessageId: headerOf(raw, "X-Dex-Message-ID"),
      sentMessageId: `<CAgmail${number}@mail.gmail.com>`,
      to: headerOf(raw, "To"),
      subject: headerOf(raw, "Subject"),
      gmailId: `gmail-${number}`,
    };
    this.sends.push(send);
    return send;
  }

  /** Gmail's answer to an accepted send of `raw`. */
  public accept(raw: string): ProviderResponse {
    const send = this.send(raw);
    return json(200, { id: send.gmailId, threadId: `thread-${send.gmailId}`, labelIds: ["SENT"] });
  }

  /**
   * A transport answer: `send` answers each messages.send request with the message it carries, and the mailbox
   * answers every other request.
   */
  public answering(send: (raw: string) => ProviderResponse | Promise<ProviderResponse>): (request: ProviderRequest) => ProviderResponse | Promise<ProviderResponse> {
    return (request) => (isGmailSend(request) ? send(rawOf(request)) : this.answer(request));
  }

  /** Answers a message search or a message read; any other request is 404. */
  public answer(request: ProviderRequest): ProviderResponse {
    const url = new URL(request.url);
    if (request.method !== "GET" || !url.pathname.includes("/users/me/messages")) {
      return notFound();
    }
    if (url.pathname.endsWith("/users/me/messages")) {
      return json(200, this.search(url.searchParams));
    }
    return this.read(decodeURIComponent(url.pathname.slice(url.pathname.lastIndexOf("/") + 1)), url.searchParams);
  }

  /**
   * The Sent copies a query finds, newest first: rfc822msgid: matches a Sent copy's own Message-ID, and in:sent lists
   * Sent Mail, narrowed by to:. Gmail leaves messages out when none match.
   */
  private search(query: URLSearchParams): Record<string, unknown> {
    const q = query.get("q") ?? "";
    this.searches.push(q);
    const limit = Number(query.get("maxResults") ?? "100") || 100;
    const byMessageId = /rfc822msgid:(\S+)/.exec(q)?.[1];
    const recipient = /(?:^| )to:(\S+)/.exec(q)?.[1];
    const found = [...this.sends].reverse().filter((send) => {
      if (byMessageId !== undefined) {
        return byMessageId.replace(/^<|>$/g, "") === send.sentMessageId.replace(/^<|>$/g, "");
      }
      return q.includes("in:sent") && (recipient === undefined || send.to.toLowerCase().includes(recipient.toLowerCase()));
    });
    const messages = found.slice(0, limit).map((send) => ({ id: send.gmailId, threadId: `thread-${send.gmailId}` }));
    return messages.length === 0 ? { resultSizeEstimate: 0 } : { messages, resultSizeEstimate: messages.length };
  }

  /** A message resource with its headers: those metadataHeaders names, for format=metadata. */
  private read(id: string, query: URLSearchParams): ProviderResponse {
    this.headerReads.push(id);
    const send = this.sends.find((candidate) => candidate.gmailId === id);
    if (send === undefined) {
      return notFound();
    }
    let headers = [
      { name: "Message-ID", value: send.sentMessageId },
      { name: "X-Dex-Message-ID", value: send.dexMessageId },
      { name: "To", value: send.to },
      { name: "Subject", value: send.subject },
    ];
    const wanted = query.getAll("metadataHeaders").map((name) => name.toLowerCase());
    if (query.get("format") === "metadata" && wanted.length > 0) {
      headers = headers.filter((header) => wanted.includes(header.name.toLowerCase()));
    }
    return json(200, { id: send.gmailId, threadId: `thread-${send.gmailId}`, labelIds: ["SENT"], payload: { mimeType: "text/plain", headers } });
  }
}
