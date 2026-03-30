package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	version         = "1.0.1"
	redirectURI     = "http://127.0.0.1:8888/callback"
	scopes          = "user-read-playback-state user-modify-playback-state user-read-currently-playing"
	spotifyAuthURL  = "https://accounts.spotify.com/authorize"
	spotifyTokenURL = "https://accounts.spotify.com/api/token"
	apiBase         = "https://api.spotify.com/v1"
)

// ── Token storage ────────────────────────────────────────────────────────────

type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "spotify-cli")
	os.MkdirAll(dir, 0700)
	return dir
}

func tokenPath() string {
	return filepath.Join(configDir(), "tokens.json")
}

func saveTokens(t *Tokens) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tokenPath(), data, 0600)
}

func loadTokens() (*Tokens, error) {
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return nil, err
	}
	var t Tokens
	return &t, json.Unmarshal(data, &t)
}

// ── OAuth flow ───────────────────────────────────────────────────────────────

func randomState() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func authenticate(clientID, clientSecret string) (*Tokens, error) {
	state := randomState()

	params := url.Values{
		"client_id":     {clientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {scopes},
		"state":         {state},
	}
	authURL := spotifyAuthURL + "?" + params.Encode()

	fmt.Println("Opening browser for Spotify authorization...")
	fmt.Println("If the browser doesn't open, visit:")
	fmt.Println(" ", authURL)

	openBrowser(authURL)

	code, err := captureCallback(state)
	if err != nil {
		return nil, err
	}

	tokens, err := tokenRequest(clientID, clientSecret, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	})
	if err != nil {
		return nil, err
	}
	return tokens, nil
}

func openBrowser(u string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{u}
	case "windows":
		cmd, args = "cmd", []string{"/c", "start", u}
	default:
		cmd, args = "xdg-open", []string{u}
	}
	exec.Command(cmd, args...).Start()
}

func captureCallback(expectedState string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:8888")
	if err != nil {
		return "", fmt.Errorf("cannot listen on :8888 (is it in use?): %w", err)
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("state") != expectedState {
				errCh <- fmt.Errorf("state mismatch — possible CSRF")
				http.Error(w, "State mismatch", http.StatusBadRequest)
				return
			}
			if e := q.Get("error"); e != "" {
				errCh <- fmt.Errorf("authorization denied: %s", e)
				fmt.Fprintf(w, "<html><body><h2>Authorization failed: %s</h2><p>You can close this tab.</p></body></html>", e)
				return
			}
			code := q.Get("code")
			if code == "" {
				errCh <- fmt.Errorf("no authorization code in callback")
				http.Error(w, "Missing code", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, "<html><body><h2>Authorization successful!</h2><p>You can close this tab and return to your terminal.</p></body></html>")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			codeCh <- code
		}),
	}

	go srv.Serve(ln)
	fmt.Println("Waiting for authorization (2 min timeout)...")

	select {
	case code := <-codeCh:
		srv.Close()
		return code, nil
	case err := <-errCh:
		srv.Close()
		return "", err
	case <-time.After(2 * time.Minute):
		srv.Close()
		return "", fmt.Errorf("authorization timed out")
	}
}

func tokenRequest(clientID, clientSecret string, data url.Values) (*Tokens, error) {
	req, err := http.NewRequest("POST", spotifyTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Error != "" {
		return nil, fmt.Errorf("%s: %s", result.Error, result.ErrorDesc)
	}

	return &Tokens{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}

// ── API client ───────────────────────────────────────────────────────────────

type client struct {
	tokens       *Tokens
	clientID     string
	clientSecret string
}

func (c *client) accessToken() (string, error) {
	if time.Now().Before(c.tokens.ExpiresAt.Add(-30 * time.Second)) {
		return c.tokens.AccessToken, nil
	}
	fresh, err := tokenRequest(c.clientID, c.clientSecret, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.tokens.RefreshToken},
	})
	if err != nil {
		return "", fmt.Errorf("token refresh failed: %w", err)
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = c.tokens.RefreshToken
	}
	c.tokens = fresh
	saveTokens(fresh)
	return fresh.AccessToken, nil
}

func (c *client) request(method, path string, body io.Reader) (*http.Response, error) {
	token, err := c.accessToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, apiBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func (c *client) get(path string) (*http.Response, error) {
	return c.request("GET", path, nil)
}

func (c *client) put(path string, body io.Reader) (*http.Response, error) {
	return c.request("PUT", path, body)
}

func (c *client) post(path string) (*http.Response, error) {
	return c.request("POST", path, nil)
}

func checkStatus(resp *http.Response, ok ...int) error {
	for _, code := range ok {
		if resp.StatusCode == code {
			return nil
		}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		// Try to extract Spotify's error message
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("spotify: %s", e.Error.Message)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ── Spotify types ────────────────────────────────────────────────────────────

type PlaybackState struct {
	IsPlaying bool `json:"is_playing"`
	ProgressMs int `json:"progress_ms"`
	Item       *struct {
		Name       string `json:"name"`
		DurationMs int    `json:"duration_ms"`
		Artists    []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Album struct {
			Name string `json:"name"`
		} `json:"album"`
	} `json:"item"`
	Device *Device `json:"device"`
}

type Device struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	IsActive      bool   `json:"is_active"`
	IsRestricted  bool   `json:"is_restricted"`
	VolumePercent int    `json:"volume_percent"`
}

// ── Commands ─────────────────────────────────────────────────────────────────

func cmdStatus(c *client) error {
	resp, err := c.get("/me/player")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		fmt.Println("No active playback session.")
		return nil
	}

	var state PlaybackState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return err
	}

	if state.Item == nil {
		fmt.Println("Nothing is currently playing.")
		return nil
	}

	symbol := "▶"
	if !state.IsPlaying {
		symbol = "⏸"
	}

	artists := make([]string, len(state.Item.Artists))
	for i, a := range state.Item.Artists {
		artists[i] = a.Name
	}

	prog := state.ProgressMs / 1000
	dur := state.Item.DurationMs / 1000

	fmt.Printf("%s  %s\n", symbol, state.Item.Name)
	fmt.Printf("    %s\n", strings.Join(artists, ", "))
	fmt.Printf("    %s\n", state.Item.Album.Name)
	fmt.Printf("    %d:%02d / %d:%02d\n", prog/60, prog%60, dur/60, dur%60)
	if state.Device != nil {
		fmt.Printf("    Device: %s (%s) — volume %d%%\n",
			state.Device.Name, state.Device.Type, state.Device.VolumePercent)
	}
	return nil
}

func cmdPlay(c *client, uri string) error {
	var body io.Reader
	if uri != "" {
		data, _ := json.Marshal(map[string]interface{}{"uris": []string{uri}})
		body = strings.NewReader(string(data))
	}
	resp, err := c.put("/me/player/play", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, 204, 202, 200); err != nil {
		return err
	}
	if uri != "" {
		fmt.Printf("Playing %s\n", uri)
	} else {
		fmt.Println("Playing")
	}
	return nil
}

func cmdPause(c *client) error {
	resp, err := c.put("/me/player/pause", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, 204, 202, 200); err != nil {
		return err
	}
	fmt.Println("Paused")
	return nil
}

func cmdToggle(c *client) error {
	resp, err := c.get("/me/player")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return cmdPlay(c, "")
	}

	var state PlaybackState
	json.NewDecoder(resp.Body).Decode(&state)

	if state.IsPlaying {
		return cmdPause(c)
	}
	return cmdPlay(c, "")
}

func cmdNext(c *client) error {
	resp, err := c.post("/me/player/next")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, 204, 202, 200); err != nil {
		return err
	}
	fmt.Println("⏭  Next track")
	return nil
}

func cmdPrev(c *client) error {
	resp, err := c.post("/me/player/previous")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, 204, 202, 200); err != nil {
		return err
	}
	fmt.Println("⏮  Previous track")
	return nil
}

func cmdVolume(c *client, vol int) error {
	if vol < 0 || vol > 100 {
		return fmt.Errorf("volume must be 0–100")
	}
	resp, err := c.put(fmt.Sprintf("/me/player/volume?volume_percent=%d", vol), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, 204, 202, 200); err != nil {
		return err
	}
	fmt.Printf("Volume: %d%%\n", vol)
	return nil
}

func listDevices(c *client) ([]Device, error) {
	resp, err := c.get("/me/player/devices")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Devices []Device `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Devices, nil
}

func cmdDevices(c *client) error {
	devices, err := listDevices(c)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Println("No devices found. Open Spotify on a device first.")
		return nil
	}
	fmt.Println("Devices:")
	for i, d := range devices {
		marker := "  "
		if d.IsActive {
			marker = "* "
		}
		fmt.Printf("  %s%d. %s  [%s]\n", marker, i+1, d.Name, d.Type)
	}
	fmt.Println("\n(* = active device)")
	return nil
}

func cmdSwitch(c *client, nameArg string) error {
	devices, err := listDevices(c)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return fmt.Errorf("no devices found — open Spotify on a device first")
	}

	var target *Device

	if nameArg != "" {
		lower := strings.ToLower(nameArg)
		for i := range devices {
			if strings.Contains(strings.ToLower(devices[i].Name), lower) {
				target = &devices[i]
				break
			}
		}
		if target == nil {
			return fmt.Errorf("no device matching %q", nameArg)
		}
	} else {
		fmt.Println("Available devices:")
		for i, d := range devices {
			marker := "  "
			if d.IsActive {
				marker = "* "
			}
			fmt.Printf("  %s%d. %s  [%s]\n", marker, i+1, d.Name, d.Type)
		}
		fmt.Print("\nSelect device number: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		n, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil || n < 1 || n > len(devices) {
			return fmt.Errorf("invalid selection")
		}
		target = &devices[n-1]
	}

	data, _ := json.Marshal(map[string]interface{}{
		"device_ids": []string{target.ID},
		"play":       true,
	})
	resp, err := c.put("/me/player", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, 204, 202, 200); err != nil {
		return err
	}

	fmt.Printf("Switched to %s\n", target.Name)
	return nil
}

// ── Entry point ───────────────────────────────────────────────────────────────

func usage() {
	fmt.Print(`spotify-cli — Control Spotify from your terminal

USAGE
  spotify-cli <command> [args]

COMMANDS
  auth              Authenticate with Spotify (run this first)
  version           Show version
  status            Show currently playing track
  play [uri]        Resume playback, or play a Spotify URI
  pause             Pause playback
  toggle            Toggle play/pause
  next              Skip to next track
  prev              Go to previous track
  volume <0-100>    Set volume
  devices           List available devices
  switch [name]     Transfer playback to a device (interactive if no name given)

SETUP
  1. Create a Spotify app at https://developer.spotify.com/dashboard
  2. Add  http://127.0.0.1:8888/callback  as a Redirect URI in your app settings
  3. Export your credentials:
       export SPOTIFY_CLIENT_ID=<your_client_id>
       export SPOTIFY_CLIENT_SECRET=<your_client_secret>
  4. Run:  spotify-cli auth

INSTALL
  go install github.com/yourusername/spotify-cli@latest

`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	clientID := os.Getenv("SPOTIFY_CLIENT_ID")
	clientSecret := os.Getenv("SPOTIFY_CLIENT_SECRET")

	cmd := os.Args[1]

	// auth doesn't need stored tokens
	if cmd == "auth" {
		if clientID == "" || clientSecret == "" {
			fmt.Fprintln(os.Stderr, "error: SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET must be set")
			os.Exit(1)
		}
		tokens, err := authenticate(clientID, clientSecret)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if err := saveTokens(tokens); err != nil {
			fmt.Fprintln(os.Stderr, "error saving tokens:", err)
			os.Exit(1)
		}
		fmt.Println("Authentication successful! Tokens saved to", tokenPath())
		return
	}

	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		usage()
		return
	}

	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		fmt.Println("spotify-cli v" + version)
		return
	}

	// All other commands require credentials + saved tokens
	if clientID == "" || clientSecret == "" {
		fmt.Fprintln(os.Stderr, "error: SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET must be set")
		os.Exit(1)
	}

	tokens, err := loadTokens()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: not authenticated — run: spotify-cli auth")
		os.Exit(1)
	}

	c := &client{
		tokens:       tokens,
		clientID:     clientID,
		clientSecret: clientSecret,
	}

	var runErr error
	switch cmd {
	case "status":
		runErr = cmdStatus(c)
	case "play":
		uri := ""
		if len(os.Args) > 2 {
			uri = os.Args[2]
		}
		runErr = cmdPlay(c, uri)
	case "pause":
		runErr = cmdPause(c)
	case "toggle":
		runErr = cmdToggle(c)
	case "next":
		runErr = cmdNext(c)
	case "prev", "previous":
		runErr = cmdPrev(c)
	case "volume":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: spotify-cli volume <0-100>")
			os.Exit(1)
		}
		vol, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %q is not a valid volume\n", os.Args[2])
			os.Exit(1)
		}
		runErr = cmdVolume(c, vol)
	case "devices":
		runErr = cmdDevices(c)
	case "switch":
		name := ""
		if len(os.Args) > 2 {
			name = strings.Join(os.Args[2:], " ")
		}
		runErr = cmdSwitch(c, name)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		usage()
		os.Exit(1)
	}

	if runErr != nil {
		fmt.Fprintln(os.Stderr, "error:", runErr)
		os.Exit(1)
	}
}
