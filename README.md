# telex

MCP server that lets agents use your **personal Telegram account** (MTProto via
[gotd/td](https://github.com/gotd/td)), written in Go.

Tools: `get_me`, `list_chats`, `get_messages`, `search_messages`,
`wait_for_messages`, `download_media`, plus (unless `TELEX_READ_ONLY=1`)
`send_message`, `send_file`, `send_reaction`, `mark_read`.

## Setup

```sh
go build -o ~/.local/bin/telex .
telex login            # api_id/hash → Keychain, then phone + code + 2FA
```

Run **one** shared server so every agent uses the same session (two processes
on one session get it revoked by Telegram; a lock file prevents that):

```sh
telex serve --http 127.0.0.1:8765
claude mcp add --scope user --transport http telegram http://127.0.0.1:8765/
```

`telex serve` with no flags speaks stdio, for a single client.

## Configuration (env)

| Variable | Meaning |
|---|---|
| `TG_APP_ID`, `TG_API_HASH` | Override the Keychain credentials |
| `TELEX_HOME` | State dir (default `~/.local/share/telex`, 0700) |
| `TELEX_ALLOWED_ROOTS` | Dirs `send_file` may read (comma/colon separated). Default: only the download dir |
| `TELEX_DOWNLOAD_DIR` | Where `download_media` writes (default `$TELEX_HOME/downloads`) |
| `TELEX_ALLOWED_CHATS` | Restrict everything to these chat IDs / @usernames |
| `TELEX_READ_ONLY` | `1` hides all write tools |

## Design

- Standard gotd wiring: `peers.Manager` → `updates.Manager` (gap recovery) →
  dispatcher; result hooks keep pts consistent for our own sends.
- Access hashes persist in bbolt, so chat IDs resolve after restarts.
- Flood waits ≤90s are absorbed by `floodwait.Waiter`; longer ones are returned
  to the agent. All RPCs are smoothed by a rate limiter; writes are capped at
  bursts of 3 then 1 per 3s.
- Chats are addressed by marked ID (`123` user, `-123` group, `-100…` channel),
  `@username`, link, or `me` — never by fuzzy display name.
- `wait_for_messages` long-polls an in-memory event log (`boot` + `seq`;
  `reset`/`gap` tell the client to re-read with `get_messages`).
- Every send/file/reaction is appended to `$TELEX_HOME/audit.jsonl` (0600).
- Revoke access any time: Telegram → Settings → Devices → "telex".
