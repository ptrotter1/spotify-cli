# spotify-cli

Control Spotify from your terminal. Zero external dependencies — built with Go's standard library only.

## Installation

Requires [Go 1.21+](https://go.dev/dl/).

```bash
go install github.com/ptrotter1/spotify-cli@latest
```

This places the `spotify-cli` binary in your `$GOPATH/bin` (or `$GOBIN`). Make sure that directory is in your `PATH`.

## Setup

### 1. Create a Spotify app

1. Go to the [Spotify Developer Dashboard](https://developer.spotify.com/dashboard)
2. Click **Create app**
3. Under **Redirect URIs**, add: `http://localhost:8888/callback`
4. Save

### 2. Set environment variables

```bash
export SPOTIFY_CLIENT_ID=your_client_id
export SPOTIFY_CLIENT_SECRET=your_client_secret
```

Add these to your `~/.zshrc` or `~/.bashrc` to persist them across sessions.

### 3. Authenticate

```bash
spotify-cli auth
```

This opens a browser window to authorize the app. Once approved, tokens are saved to `~/.config/spotify-cli/tokens.json` and refreshed automatically.

## Usage

```
spotify-cli <command> [args]
```

| Command | Description |
|---|---|
| `auth` | Authenticate with Spotify |
| `status` | Show currently playing track |
| `play [uri]` | Resume playback, or play a Spotify URI |
| `pause` | Pause playback |
| `toggle` | Toggle play/pause |
| `next` | Skip to next track |
| `prev` | Go to previous track |
| `volume <0-100>` | Set volume |
| `devices` | List available devices |
| `switch [name]` | Transfer playback to a device |

### Examples

```bash
# See what's playing
spotify-cli status

# Play/pause
spotify-cli toggle

# Skip tracks
spotify-cli next
spotify-cli prev

# Set volume
spotify-cli volume 50

# Play a specific track, album, or playlist by Spotify URI
spotify-cli play spotify:track:4uLU6hMCjMI75M1A2tKUQC

# List all devices (speakers, phones, computers, etc.)
spotify-cli devices

# Switch device interactively
spotify-cli switch

# Switch device by name (fuzzy match)
spotify-cli switch "living room"
```

## How it works

Authentication uses the [OAuth 2.0 Authorization Code flow](https://developer.spotify.com/documentation/web-api/tutorials/code-flow). On first run, `spotify-cli auth` opens a browser, starts a temporary local server on port 8888 to capture the callback, exchanges the authorization code for tokens, and saves them locally. Subsequent commands refresh the access token automatically when it expires.
