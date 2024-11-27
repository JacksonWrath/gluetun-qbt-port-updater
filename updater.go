package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"time"
)

const (
	PORT_FORWARD_API = "/v1/openvpn/portforwarded"

	QBT_LOGIN_API           = "/api/v2/auth/login"
	QBT_GET_PREFERENCES_API = "/api/v2/app/preferences"
	QBT_SET_PREFERENCES_API = "/api/v2/app/setPreferences"
)

var (
	gluetunAddress      = getEnvOrDefault("GLUETUN_ADDRESS", "localhost")
	gluetunApiPort      = getEnvOrDefault("GLUETUN_API_PORT", "8000")
	qBittorrentAddress  = getEnvOrDefault("QBT_ADDRESS", "localhost")
	qBittorrentApiPort  = getEnvOrDefault("QBT_API_PORT", "8080")
	qBittorrentUser     = getEnvOrDefault("QBT_USERNAME", "") // I realize the wrapper function is redundant in these cases.
	qBittorrentPassword = getEnvOrDefault("QBT_PASSWORD", "") // Left this way for consistency.
	qBittorrentUrl      = fmt.Sprintf("http://%s:%s", qBittorrentAddress, qBittorrentApiPort)
	gluetunUrl          = fmt.Sprintf("http://%s:%s", gluetunAddress, gluetunApiPort)
)

type portForwardResponse struct {
	Port uint16 `json:"port"`
}

type qBittorrentSettings struct {
	ListenPort uint16 `json:"listen_port"`
}

func getEnvOrDefault(key string, defaultVal string) string {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val
}

func getQBTSettings(client *http.Client, cookies []*http.Cookie) (qBittorrentSettings, error) {
	// Make a new request.
	req, err := http.NewRequest(http.MethodGet, qBittorrentUrl+QBT_GET_PREFERENCES_API, nil)
	if err != nil {
		slog.Error("failed to create request", slog.Any("err", err))
		return qBittorrentSettings{}, err
	}

	// Add the cookies.
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	// Kick off the request.
	res, err := client.Do(req)
	if err != nil {
		return qBittorrentSettings{}, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return qBittorrentSettings{}, err
	}
	slog.Info("got qbittorrent preferences", slog.String("status", res.Status), slog.Any("body", body))

	settings := new(qBittorrentSettings)
	err = json.Unmarshal(body, settings)
	return *settings, err
}

func qBittorrentLogin(client *http.Client) ([]*http.Cookie, error) {
	slog.Info("logging into qbittorrent")
	res, err := client.PostForm(qBittorrentUrl+QBT_LOGIN_API, url.Values{
		"username": {qBittorrentUser},
		"password": {qBittorrentPassword}})
	if err != nil {
		slog.Error("failed qbittorrent login", slog.Any("err", err))
		return nil, err
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	} else if res.StatusCode != 200 {
		return nil, fmt.Errorf("request failed: %s", res.Status)
	}

	slog.Info("login successful", slog.Any("body", body))
	defer res.Body.Close()

	for _, c := range res.Cookies() {
		slog.Info("cookie", slog.String("cookie", c.String()))
	}
	return res.Cookies(), err
}

func getGluetunForwardedPort() (uint16, error) {
	slog.Debug("fetching forwarded port")
	url := gluetunUrl + PORT_FORWARD_API
	resp, err := http.Get(url)
	if err != nil {
		slog.Error("failed to fetch forwarded port",
			slog.String("url", url),
			slog.String("err", err.Error()))
		return 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read response body",
			slog.String("err", err.Error()))
		return 0, err
	}

	pfr := new(portForwardResponse)
	if err := json.Unmarshal(data, pfr); err != nil {
		slog.Error("failed to unmarshal response body",
			slog.Any("body", data),
			slog.String("err", err.Error()))
		return 0, err
	}

	slog.Debug("successfully fetched forwarded port", slog.Int("port", int(pfr.Port)))
	return pfr.Port, nil
}

func handleChangedPort(client *http.Client, cookies []*http.Cookie, port uint16) error {
	newSettings := &qBittorrentSettings{
		ListenPort: port,
	}

	jsonBuf, err := json.Marshal(newSettings)
	if err != nil {
		slog.Error("failed to marshal settings", slog.Any("settings", newSettings))
		return err
	}

	body := bytes.NewBufferString("json=")
	body.Write(jsonBuf)
	slog.Info("setting qbittorrent preferences", slog.Any("body", body))

	var req *http.Request
	req, err = http.NewRequest(http.MethodPost, qBittorrentUrl+QBT_SET_PREFERENCES_API, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Add the cookies.
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	var res *http.Response
	res, err = client.Do(req)
	if err != nil || res.StatusCode != 200 {
		if err == nil {
			err = fmt.Errorf("request failed: %s", res.Status)
		}
		slog.Error("failed to set qbittorrent preferences", slog.Any("err", err))
		return err
	}

	slog.Info("successfully updated listen port")
	return nil
}

func main() {
	interval := *flag.Duration("interval", time.Second, "how often to check the API")
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Fatal(err.Error())
	}

	client := &http.Client{
		Jar: jar,
	}

	// Login to qbittorrent if needed and get cookies for later use.
	cookies := []*http.Cookie{}
	// qBittorrent allows skipping auth for local clients.
	// Assume this is intended if no username is set.
	if qBittorrentUser != "" {
		cookies, err = qBittorrentLogin(client)
		if err != nil {
			log.Fatal(err.Error())
		}
	}

	var settings qBittorrentSettings
	settings, err = getQBTSettings(client, cookies)
	if err != nil {
		log.Fatal(err.Error())
	}

	slog.Info("starting the port-forward watcher",
		slog.String("gluetun_url", gluetunUrl+PORT_FORWARD_API),
		slog.Duration("interval", interval))

	// Last observed forwaded port.
	lastPort := settings.ListenPort

	t := time.NewTicker(interval)
	for range t.C {
		// Fetch the forwarded port.
		port, err := getGluetunForwardedPort()
		if err != nil {
			log.Fatal(err.Error())
		}

		slog.Debug("got forwarded port",
			slog.Int("old", int(lastPort)),
			slog.Int("new", int(port)),
			slog.Bool("changed", lastPort != port))

		if lastPort != port {
			slog.Info("port changed, handling", slog.Int("old", int(lastPort)), slog.Int("new", int(port)))

			lastPort = port
			if err := handleChangedPort(client, cookies, port); err != nil {
				log.Fatal(err.Error())
			}
		}
	}
}
