// Command demodata builds a fake listening history for README screenshots.
//
//	go run ./cmd/demodata spotify <config-dir> <api-path>
//
// prints the JSON of one Spotify Web API GET (e.g. /tracks/<id>), using the
// Spotify keys stored in <config-dir>. Dev tool only.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"younithm/internal/db"
	"younithm/internal/secrets"
)

func spotifyGet(dir, path string) error {
	ctx := context.Background()
	pools, err := db.OpenPools(dir)
	if err != nil {
		return err
	}
	defer pools.Close()
	box, _, err := secrets.OpenBox(secrets.KeyOptions{ConfigDir: dir})
	if err != nil {
		return err
	}
	st := secrets.NewStore(pools.Read, pools.Write, box, nil)
	id, err := st.Get(ctx, "spotify.client_id")
	if err != nil {
		return err
	}
	secret, err := st.Get(ctx, "spotify.client_secret")
	if err != nil {
		return err
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://accounts.spotify.com/api/token", strings.NewReader(form.Encode()))
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.AccessToken == "" {
		return fmt.Errorf("token: status %d", resp.StatusCode)
	}

	req, _ = http.NewRequestWithContext(ctx, "GET", "https://api.spotify.com/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept-Language", "en")
	resp, err = hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

var (
	stdout   io.Writer = os.Stdout
	stderr   io.Writer = os.Stderr
	commands           = map[string]func(args []string) error{
		"spotify": func(args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("usage: demodata spotify <config-dir> <api-path>")
			}
			return spotifyGet(args[0], args[1])
		},
	}
)

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: demodata generate > demo.jsonl | demodata spotify <config-dir> <api-path>")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
