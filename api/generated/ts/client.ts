// Konoha Console API — TypeScript client.
//
// The endpoint/schema TYPES in ./api.gen.ts are generated from
// api/openapi.yaml by `make api` (openapi-typescript). This thin fetch wrapper
// is written once against those generated types (no runtime dependency, global
// `fetch`), so it runs in Node (>=22) and browsers.
import type { components, paths } from "./api.gen.ts";

export type ConsoleStatus = components["schemas"]["ConsoleStatus"];
export type CycleSummary = components["schemas"]["CycleSummary"];
export type CycleListResponse = components["schemas"]["CycleListResponse"];
export type CycleDetail = components["schemas"]["CycleDetail"];
export type ArtifactListResponse = components["schemas"]["ArtifactListResponse"];
export type QueueView = components["schemas"]["QueueView"];
export type DecisionRequest = components["schemas"]["DecisionRequest"];
export type DecisionResult = components["schemas"]["DecisionResult"];

// MutationPath is the subset of paths carrying a POST action.
type MutationPath = Extract<keyof paths, `/api/v1/cycles/{id}/${string}`>;

export interface ConsoleClientOptions {
  /** Loopback base URL, e.g. http://127.0.0.1:8765 */
  baseUrl: string;
  /** Bearer token (KONOHA_CONSOLE_TOKEN). */
  bearer?: string;
  /** Per-process CSRF token from GET /api/v1/status (browsers). */
  csrf?: string;
  /** Injectable fetch (defaults to the global). */
  fetch?: typeof fetch;
}

/** ConsoleAPIError is a non-2xx console response. */
export class ConsoleAPIError extends Error {
  readonly status: number;
  readonly body: string;

  constructor(status: number, body: string) {
    super(`console API: HTTP ${status}: ${body}`);
    this.status = status;
    this.body = body;
  }
}

export class ConsoleClient {
  private readonly opts: ConsoleClientOptions;
  private readonly fetchFn: typeof fetch;

  constructor(opts: ConsoleClientOptions) {
    this.opts = opts;
    this.fetchFn = opts.fetch ?? fetch;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (this.opts.bearer) headers.Authorization = `Bearer ${this.opts.bearer}`;
    if (this.opts.csrf) headers["X-Konoha-CSRF"] = this.opts.csrf;
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await this.fetchFn(this.opts.baseUrl + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    if (!res.ok) throw new ConsoleAPIError(res.status, text);
    return text === "" ? (undefined as T) : (JSON.parse(text) as T);
  }

  /** GET /api/v1/status (unauthenticated bootstrap). */
  getStatus(): Promise<ConsoleStatus> {
    return this.request("GET", "/api/v1/status");
  }

  /** GET /api/v1/cycles */
  listCycles(): Promise<CycleListResponse> {
    return this.request("GET", "/api/v1/cycles");
  }

  /** GET /api/v1/cycles/{id} */
  getCycle(id: string): Promise<CycleDetail> {
    return this.request("GET", `/api/v1/cycles/${encodeURIComponent(id)}`);
  }

  /** GET /api/v1/cycles/{id}/artifacts */
  listArtifacts(id: string): Promise<ArtifactListResponse> {
    return this.request("GET", `/api/v1/cycles/${encodeURIComponent(id)}/artifacts`);
  }

  /** GET /api/v1/queue */
  getQueue(): Promise<QueueView> {
    return this.request("GET", "/api/v1/queue");
  }

  /** POST /api/v1/cycles/{id}/decisions */
  submitDecision(id: string, body: DecisionRequest): Promise<DecisionResult> {
    return this.request("POST", `/api/v1/cycles/${encodeURIComponent(id)}/decisions`, body);
  }
}

// mutationPaths lists the generated POST action paths, so the client and the
// generated spec stay visibly linked.
export const mutationPaths: MutationPath[] = [
  "/api/v1/cycles/{id}/decisions",
  "/api/v1/cycles/{id}/forms",
  "/api/v1/cycles/{id}/actions",
];
