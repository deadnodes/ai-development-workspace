# Bounded browser and agent context

The full operator snapshot `/api/state` and backup remain historical exports.
They are not polling or agent discovery interfaces. All original records and
append-only events remain stored; projections/indexes do not delete history.

The browser uses `/api/ui-state` and scopes requests to the selected Product and
Feature. Its persistence reader excludes unrelated histories before JSON decoding.
History windows are explicit in response metadata. Navigation and live reads use
the same scope; responses from a prior route cannot replace the current view.

Agents should navigate with `get_state`, find relevant source cards with
`search_context`, follow typed persisted links with `graph_neighbors`, and request
only the needed text using `get_context_excerpt`. MCP `resume` returns a compact
checkpoint summary rather than an entire Feature archive. Full HTTP Feature
context remains a separate operator contract. Search cards and excerpts carry
source IDs and explicit limits/truncation; search rank is not factual certainty,
release readiness or execution authorization.

PostgreSQL full-text indexing covers curated Feature, Integration and Memory
text, never arbitrary event payloads, credentials or provider logs. Typed graph
edges derive from existing ownership, parent and dependency/repository references.
No inferred semantic edge is presented as a recorded relationship. Product
permissions apply before retrieval; cross-product graph expansion is excluded.
Derived indexes can be rebuilt from source records and are maintained on writes.

## Memorilla assessment

Reference: https://github.com/snap-stanford/memorilla (Apache-2.0), inspected
2026-11-08. Memorilla is a trained query-dependent latent-memory compressor for a
frozen language model. Its published workflow requires Linux/CUDA models; it is
not a drop-in graph database or lexical search implementation for this Go service.
No Memorilla source or model weights are included here. RCP adopts the useful
query-dependent bounded-context principle while preserving human-readable,
provenance-linked source excerpts using local PostgreSQL text indexes and typed
relationships. This avoids a new GPU/model dependency for ordinary navigation.

## Example agent workflow

```json
{"tool":"search_context","arguments":{"product_id":"deadnodes","query":"Smart Review","limit":5}}
{"tool":"graph_neighbors","arguments":{"product_id":"rcp","id":"rcp-live-ui","depth":2,"limit":10}}
{"tool":"get_context_excerpt","arguments":{"product_id":"rcp","id":"rcp-live-ui","offset":0,"max_chars":1200}}
```

Search and graph output have a64KiB serialized budget. Source excerpts use Unicode
character offsets. Full-text ranking indexes the first32768 source characters;
exact substring fallback and paged excerpts cover the complete authored text.
Curated knowledge nodes remain a separate graph: `search_product_knowledge`
returns bounded node snippets, while full node text stays in the explicit operator
HTTP project-context API. Do not pass curated node IDs to the authored-record
excerpt/neighbor tools.

The visual delivery graph and attention report require explicit loading in the
bounded browser view; they do not silently start complete-history reads on every
poll. They retain their existing full evidence semantics when explicitly opened.
