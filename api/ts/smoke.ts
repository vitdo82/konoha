import { createServer } from "node:http";
import { ConsoleClient } from "../generated/ts/client.ts";

// Smoke test for the generated TypeScript client. It stands up a loopback
// server returning spec-shaped JSON and exercises the typed client against it.
// Run with: `npm run smoke` (requires Node >= 22).

const status = {
  name: "konoha",
  version: "test",
  readOnly: true,
  decisions: false,
  queue: false,
  stream: false,
  csrfHeader: "X-Konoha-CSRF",
  csrfToken: "csrf-token",
};
const cycles = {
  cycles: [{ id: "r-issue-1", state: "CLOSED" }],
  columns: [],
};

const server = createServer((req, res) => {
  res.setHeader("content-type", "application/json");
  switch (req.url) {
    case "/api/v1/status":
      res.end(JSON.stringify(status));
      break;
    case "/api/v1/cycles":
      res.end(JSON.stringify(cycles));
      break;
    default:
      res.statusCode = 404;
      res.end(JSON.stringify({ error: "not found" }));
  }
});

await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const address = server.address();
const port = typeof address === "object" && address ? address.port : 0;
const client = new ConsoleClient({ baseUrl: `http://127.0.0.1:${port}`, bearer: "t" });

try {
  const st = await client.getStatus();
  if (st.name !== "konoha" || st.csrfToken !== "csrf-token") {
    throw new Error(`unexpected status: ${JSON.stringify(st)}`);
  }
  const list = await client.listCycles();
  if (list.cycles[0]?.id !== "r-issue-1") {
    throw new Error(`unexpected cycles: ${JSON.stringify(list)}`);
  }
  console.log("ts client smoke OK");
} finally {
  server.close();
}
