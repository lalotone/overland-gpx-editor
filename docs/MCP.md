# MCP agent control

Overland can expose the open planner, track editor, or Explore map to MCP
clients. It is off by default:

```bash
./overland serve --mcp
```

This starts the web app on `--addr` (`127.0.0.1:8000`) and the MCP endpoint on
its own loopback listener, `--mcp-addr` (`127.0.0.1:8009`). Open the web app
before asking an agent to use a tool; the MCP server deliberately has no
detached document of its own to edit.

The agent endpoint is kept off the main listener on purpose. A reverse proxy
fronting the app cannot reach it, whatever headers it sets — the guarantee is
the socket, not a `Host` check. `--mcp-addr` refuses any non-loopback address.
Only the private browser bridge, `/mcp/browser/*`, stays on the main listener,
because the page has to reach it same-origin.

## Transport

`/mcp` on the MCP listener is the standard MCP Streamable HTTP transport,
implemented with the official Go SDK. Protocol negotiation, initialization, request framing and
cancellation follow the MCP specification; no TCP or stdio adapter is needed.
Requests and responses are limited to 4 MiB.

For OpenCode, add a remote server to `~/.config/opencode/opencode.jsonc`:

```jsonc
{
  "mcp": {
    "overland": {
      "type": "remote",
      "url": "http://127.0.0.1:8009/mcp",
      "enabled": true
    }
  }
}
```

Restart OpenCode after changing its configuration. If Overland uses a different
`--mcp-addr`, use that port in the URL — it is the MCP port, not `--addr`.

## Tools

| Tool | Effect |
| --- | --- |
| `get_view` | Read the complete active browser snapshot |
| `plan_route` | Replace or append route controls and route them with the road, mixed, or trail motorcycle profile |
| `draw_track` | Replace or append geometry on the open track; marks it dirty and remains undoable |
| `set_waypoints` | Replace or append typed or device-specific GPX waypoints without changing route controls |
| `draw_map_track` | Replace, append, or clear a session-only track overlay on any active map |
| `set_map_markers` | Replace, append, or clear typed session-only markers on any active map |
| `switch_mode` | Switch explicitly between planner, editor, and Explore |
| `set_map_view` | Pan and zoom the active planner, track, or Explore map |
| `open_track` | Open an exact library filename; refuses while the current track has unsaved edits |
| `select_track` | Select a zero-based track index in a multi-track GPX file |
| `load_pois` | Load fuel, water, or campsite data in the planning view or around the open track |

For example, with the planner already active, this draws two route controls and
lets Overland calculate the motorbike route between them:

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "plan_route",
    "arguments": {
      "points": [
        { "lat": 42.8467, "lon": -2.6726 },
        { "lat": 42.9040, "lon": -2.5310 }
      ],
      "profile": "mixed",
      "mode": "replace",
      "fitView": true
    }
  }
}
```

Route controls, GPX waypoints, and session map annotations are intentionally
separate. Use `plan_route` for places the calculated line must pass through.
Use `set_waypoints` for fuel stops, camps, warnings, and other markers that
should be saved with a GPX. Use `draw_map_track` and `set_map_markers` when the
annotation should be visible to the user but must not alter a GPX file.

### Modes

`switch_mode` uses the public names `planner`, `editor`, and `explore`.
Switching to `editor` requires an open track; `open_track` is the explicit way
to load one and enters the editor. Unsaved editor changes remain in memory when
switching away.

Mode-specific tools reject calls from other screens instead of navigating
silently:

| Mode | Tools |
| --- | --- |
| planner | `plan_route`, `set_waypoints target=planner`, `load_pois scope=current_view` |
| editor | `draw_track`, `set_waypoints target=track`, `select_track`, `load_pois scope=current_track` |
| explore | Explore has no GPX-editing tool |

`draw_map_track`, `set_map_markers`, and `set_map_view` work in all three map
modes without switching. An empty replacement clears the corresponding session
overlay. Appending is capped at 6,000 track points and 1,000 markers in total.

### Marker catalog

The stable marker IDs accepted by `set_map_markers` and by the optional
`marker` field on `set_waypoints` are:

`generic`, `fuel`, `water`, `camp`, `food`, `lodging`, `parking`, `repair`,
`medical`, `viewpoint`, `hazard`, `roadblock`, `ferry`, `border`, `restroom`,
`information`, and `picnic`.

The full catalog, including display labels, glyphs, colours, and mapped GPX
symbols, is returned by `get_view` as `markerCatalog`. `set_waypoints` also
accepts a free-form `sym` instead of `marker` for device-specific GPX symbols;
the two fields cannot be supplied together. Imported unknown symbols are
preserved even though the UI renders them with the safe generic icon.

## View resource

`overland://view` is an `application/json` resource containing the selected
browser `viewId`, its freshness, and the state visible to the user. The state
includes:

- map center, zoom, bounds, base layer, hillshade, and offline mode;
- the marker catalog and session-only map track/markers;
- planner controls, routed geometry, routing engine/status, elevation and
  surface state;
- the open track and GPX waypoints, dirty/undo state, distance, elevation, and
  time statistics;
- loaded POIs and the subset inside the current viewport;
- library filenames and summaries;
- place-search results, cursor coordinates, and ground elevation;
- `notifications`: everything the user was told, newest last.

When several tabs are open, a focused visible tab wins. Otherwise the freshest
view is used. A tab that has not checked in for five seconds is not considered
active.

## Errors

A tool call reports its own failure directly: a wrong-mode command, an unknown
filename or an invalid argument comes back as an error, and nothing changes.

Work a command *starts* can fail after it has returned. `plan_route` is the
clearest case: it stores the route controls and answers immediately, then
Overland routes, reads elevation and traces surface in the background. A
`{"ok":true}` result therefore means "the controls were accepted", not "the
route worked".

Those later failures surface in the snapshot rather than in the tool result:

- `planner.routeError` is the message from the last failed routing attempt, and
  is cleared when a new one starts. Without it a failed route is
  indistinguishable from one that was never requested — both leave
  `routedCoordinates` empty and `loading` false.
- `planner.elevationError` and `planner.surfaceError` cover the two stages that
  follow routing.
- `notifications` retains the last 20 messages shown to the user, each with a
  `seq`, an ISO `at`, a `type` (`error`, `success` or `info`) and the text. The
  toasts themselves disappear after five seconds; this log does not, so a
  polling client cannot miss one. Compare `seq` against the highest you have
  already seen to find what is new.

So after a command that starts background work, poll `overland://view` until
`planner.loading` is false, then check `planner.routeError` and any new
`notifications` entries.

Explore's own offline-pack errors are still local to that screen and are not
reported in the snapshot.

## Safety boundary

Both halves are local-only, by different means. `/mcp` is bound to a loopback
address by `--mcp-addr`, which refuses anything else, so it is unreachable from
another machine no matter how the main listener is configured; it rejects
non-loopback peers as well. The browser bridge does share the main listener, so
it keeps the header-level guards: a random process capability, a loopback HTTP
host and peer, and a refusal to serve cross-site requests even when `--addr` is
bound to a wildcard address. The capability is handed only to a same-origin Overland page, as an
HttpOnly cookie scoped to `/mcp/browser`, and is never returned by `/config`,
readable by page scripts, or logged.

Commands reach the page over a server-sent event stream at
`/mcp/browser/events`; the page still publishes its state snapshot once a
second. Because the stream carries no Authorization header, it relies on that
scoped cookie plus `SameSite=Strict`. An unacknowledged command is redelivered
when the stream reconnects, and the page replays the stored result instead of
applying the edit twice. Since the bridge depends on cookies, it requires a
same-origin page; `VITE_API_BASE` pointing at a remote backend disables it.

Enabling MCP still grants every local process that can reach the MCP listener
control of the open browser view. Run it only when local agents are trusted.

`--mcp` and `--behind-proxy` cannot be combined. A proxied deployment serves
remote browsers, so the tab an agent would drive is not on this machine and the
loopback guarantees the bridge depends on no longer hold; Overland refuses to
start rather than half-enabling it.

MCP edits do not automatically save, replace, or delete GPX files. The user
retains the explicit save boundary in the UI. Session map overlays are kept
outside planner/editor documents, dirty state, undo history, saves, and
downloads. They survive map-mode switches and disappear on browser reload.

The snapshot exposes application-managed data, not arbitrary labels or
features rendered inside the vector basemap. Planner, track-editor, and Explore
viewports all report their current center, zoom, and bounds.
