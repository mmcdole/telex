# telex

An MCP server that lets AI agents use your personal Telegram account.

## Setup

```sh
go install github.com/mmcdole/telex@latest
telex login
telex serve --http 127.0.0.1:8765
claude mcp add --transport http telex http://127.0.0.1:8765/
```

Run one shared server for all your agents. Telegram revokes a session that two
processes use at once.

## Configuration

| Variable | Meaning |
|---|---|
| `TG_APP_ID`, `TG_API_HASH` | Override the Keychain credentials |
| `TELEX_HOME` | State dir (default `~/.local/share/telex`) |
| `TELEX_ALLOWED_ROOTS` | Dirs `send_file` may read from |
| `TELEX_DOWNLOAD_DIR` | Where `download_media` writes (default `$TELEX_HOME/downloads`) |
| `TELEX_ALLOWED_CHATS` | Limit all tools to these chat IDs or @usernames |
| `TELEX_READ_ONLY` | `1` hides the tools that send or change anything |

Sends are logged to `$TELEX_HOME/audit.jsonl`. To revoke access, go to
Telegram → Settings → Devices.
