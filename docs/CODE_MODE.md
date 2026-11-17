# Agent code mode

Code mode lets agents batch multi-step work in a local Node 24 TypeScript script.
The SDK is generated from the live MCP tool registry; there is no build step or
additional dependency. The server never accepts or executes submitted code, so
there is no server sandbox. The [responsibility boundary](RESPONSIBILITY_BOUNDARY.md)
is unchanged: arbitrary code runs in the agent's environment.

Call MCP `get_code_api {"query":"resume"}` for filtered declarations and usage,
or omit `query` for all tools. Matching is case-insensitive on names/descriptions.

| Endpoint | Result |
| --- | --- |
| `GET /api/tools` | Sorted tool names, descriptions, input/output schemas |
| `GET /api/code-sdk/rcp.ts` | Downloadable typed module |
| `POST /api/tools/{name}` | Invoke the registered MCP tool with a JSON arguments object |

All endpoints use the same authentication wrappers as `/mcp`. The SDK uses
`RCP_URL` (default: the download request's origin), `RCP_TOKEN`, or a trimmed
`RCP_TOKEN_FILE` read locally with `node:fs`. An explicit token takes precedence.
The SDK sends bearer authorization; scripts have the same permissions as MCP
calls. Writes still require the existing attributed commands and policies;
code mode grants no additional authority. Configure forwarded host/protocol
headers correctly when serving behind a proxy.

```sh
curl -fsS -H "Authorization: Bearer $RCP_TOKEN" \
  "$RCP_URL/api/code-sdk/rcp.ts" -o rcp.ts
node script.ts
```

Example `script.ts` (replace IDs with the selected Product and Features):

```ts
import rcp from "./rcp.ts";

const matches = await rcp.search_context({
  product_id: "PRODUCT_ID", query: "acceptance", limit: 5,
});
const summaries = await Promise.all(
  ["FEATURE_ID_1", "FEATURE_ID_2"].map(feature_id => rcp.resume({ feature_id })),
);
console.log(JSON.stringify({ matches, summaries }));
```

Download the SDK once for a script session; refresh it when tool schemas change.
Batch independent reads with `Promise.all` and print only the final compact
result. Sequence dependent writes explicitly; batching is not an atomic transaction.

Arguments must be a JSON object; an empty body means `{}`. Bodies are capped at
12 MiB (413 on overflow). MCP validation/policy errors still apply. JSON-RPC
errors map to 400 (unknown tools to 404), tool errors to 422. Successful calls
return structured content, otherwise JSON parsed from text, otherwise `{text}`.
SDK errors include the HTTP status and server error message.

Schema conversion is deterministic and bounded to eight levels, with `any` for
unsupported/deeper schema constructs. Input types are guidance; server-side MCP
validation remains authoritative. Return values use `Promise<any>`. Node 24 native
type stripping runs the module directly; no TypeScript compiler is required.
