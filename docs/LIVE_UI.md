# Live UI state

The browser checks `/api/state` every three seconds while the tab is visible. It sends the last snapshot's `ETag` in `If-None-Match`. Unchanged state returns `304` without a JSON body; changed state returns one consistent snapshot. Authentication and the API's `Cache-Control: no-store` still apply.

The UI reconciles changed DOM nodes rather than replacing the entire page. Open details, focused controls, scroll position and unchanged nodes should survive. An open dialog or active editing pauses application of background updates; the next eligible check catches up. Hidden tabs pause polling. Network failures retain the last displayed state and recover on later checks.

This is bounded polling, not a WebSocket/SSE stream and not a delta-event protocol. A changed response includes the whole state snapshot; the DOM update is granular. It also cannot make an external Git/CI/runtime observation fresher than the observation recorded by the application.
