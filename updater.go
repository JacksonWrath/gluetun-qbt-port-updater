package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	PORT_FORWARD_API        = "/v1/openvpn/portforwarded"
	QBT_LOGIN_API           = "/api/v2/auth/login"
	QBT_GET_PREFERENCES_API = "/api/v2/app/preferences"
	QBT_SET_PREFERENCES_API = "/api/v2/app/setPreferences"

	LOG_LEVEL_KEY = "LOG_LEVEL"
)

var (
	gluetunAddress      = getEnvOrDefault("GLUETUN_ADDRESS", "localhost")
	gluetunApiPort      = getEnvOrDefault("GLUETUN_API_PORT", "8000")
	qBittorrentAddress  = getEnvOrDefault("QBT_ADDRESS", "localhost")
	qBittorrentApiPort  = getEnvOrDefault("QBT_API_PORT", "8080")
	qBittorrentUser     = getEnvOrDefault("QBT_USERNAME", "") // I realize the wrapper function is redundant in these cases.
	qBittorrentPassword = getEnvOrDefault("QBT_PASSWORD", "") // Left this way for consistency.

	qBittorrentUrl = fmt.Sprintf("http://%s:%s", qBittorrentAddress, qBittorrentApiPort)
	gluetunUrl     = fmt.Sprintf("http://%s:%s", gluetunAddress, gluetunApiPort)
	logger         = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevelFromEnv()}))
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

func logLevelFromEnv() slog.Level {
	envLogLevel := strings.ToUpper(getEnvOrDefault(LOG_LEVEL_KEY, "INFO"))
	levelVar := &slog.LevelVar{}
	err := levelVar.UnmarshalText([]byte(envLogLevel))
	if err != nil {
		log.Fatal("Invalid LOG_LEVEL: ", err.Error())
	}
	return levelVar.Level()
}

func getQBTSettings(client *http.Client, cookies []*http.Cookie) (qBittorrentSettings, error) {
	// Make a new request.
	req, err := http.NewRequest(http.MethodGet, qBittorrentUrl+QBT_GET_PREFERENCES_API, nil)
	if err != nil {
		logger.Error("failed to create request", slog.Any("err", err))
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
	logger.Debug("got qbittorrent preferences", slog.String("status", res.Status), slog.Any("body", body))

	settings := new(qBittorrentSettings)
	err = json.Unmarshal(body, settings)
	return *settings, err
}

func qBittorrentLogin(client *http.Client) ([]*http.Cookie, error) {
	logger.Info("logging into qbittorrent")
	res, err := client.PostForm(qBittorrentUrl+QBT_LOGIN_API, url.Values{
		"username": {qBittorrentUser},
		"password": {qBittorrentPassword}})
	if err != nil {
		logger.Error("failed qbittorrent login", slog.Any("err", err))
		return nil, err
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	} else if res.StatusCode != 200 {
		return nil, fmt.Errorf("request failed: %s", res.Status)
	}

	logger.Info("login successful", slog.Any("body", body))
	defer res.Body.Close()

	for _, c := range res.Cookies() {
		logger.Info("cookie", slog.String("cookie", c.String()))
	}
	return res.Cookies(), err
}

func getGluetunForwardedPort() (uint16, error) {
	logger.Debug("fetching forwarded port")
	url := gluetunUrl + PORT_FORWARD_API
	resp, err := http.Get(url)
	if err != nil {
		logger.Error("failed to fetch forwarded port",
			slog.String("url", url),
			slog.String("err", err.Error()))
		return 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("failed to read response body",
			slog.String("err", err.Error()))
		return 0, err
	}

	pfr := new(portForwardResponse)
	if err := json.Unmarshal(data, pfr); err != nil {
		logger.Error("failed to unmarshal response body",
			slog.Any("body", data),
			slog.String("err", err.Error()))
		return 0, err
	}

	logger.Debug("successfully fetched forwarded port", slog.Int("forwarded-port", int(pfr.Port)))
	return pfr.Port, nil
}

func handleChangedPort(client *http.Client, cookies []*http.Cookie, port uint16) error {
	newSettings := &qBittorrentSettings{
		ListenPort: port,
	}

	jsonBuf, err := json.Marshal(newSettings)
	if err != nil {
		logger.Error("failed to marshal settings", slog.Any("settings", newSettings))
		return err
	}

	body := bytes.NewBufferString("json=")
	body.Write(jsonBuf)
	logger.Debug("setting qbittorrent preferences", slog.String("body", body.String()))

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
		logger.Error("failed to set qbittorrent preferences", slog.Any("err", err))
		return err
	}

	logger.Info("successfully updated listen port")
	return nil
}

func waitForConnUp(address string) {
	interval := time.Duration(time.Second)
	t := time.NewTicker(interval)
	for range t.C {
		conn, err := net.DialTimeout("tcp", address, time.Second*5)
		if err == nil {
			defer conn.Close()
			break
		}
		logger.Debug("Failed to connect", slog.String("address", address), slog.Any("err", err.Error()))
	}
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

	logger.Info("Checking if qBittorrent is up")
	waitForConnUp(fmt.Sprintf("%s:%s", qBittorrentAddress, qBittorrentApiPort))
	logger.Info("Checking if Gluetun is up")
	waitForConnUp(fmt.Sprintf("%s:%s", gluetunAddress, gluetunApiPort))

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

	logger.Info("starting the port-forward watcher",
		slog.String("gluetun_url", gluetunUrl+PORT_FORWARD_API),
		slog.Duration("interval", interval))

	// Last observed forwaded port.
	qbtPort := settings.ListenPort
	lastPort, err := getGluetunForwardedPort()
	if err != nil {
		log.Fatal(err.Error())
	}

	logger.Info("Current port set in qBittorrent", slog.Int("qbt-port", int(qbtPort)))
	logger.Info("Current forwarded port from Gluetun", slog.Int("forwarded-port", int(lastPort)))

	t := time.NewTicker(interval)
	for range t.C {
		// Fetch the forwarded port.
		port, err := getGluetunForwardedPort()
		if err != nil {
			log.Fatal(err.Error())
		}

		if lastPort != port {
			logger.Info("Forwarded port from Gluetun changed", slog.Int("previous-port", int(lastPort)), slog.Int("forwarded-port", int(port)))
			lastPort = port
		}
		if port != qbtPort && port != 0 {
			logger.Info("Updating qBittorrent port", slog.Int("qbt-port", int(qbtPort)), slog.Int("forwarded-port", int(port)))
			if err := handleChangedPort(client, cookies, port); err != nil {
				log.Fatal(err.Error())
			}
			qbtPort = port
		}
	}
}
