package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/google/uuid"
)

type Config struct {
	SwitchBotToken  string
	SwitchBotSecret string
	MastodonURL     string
	MastodonToken   string
}

type Device struct {
	DeviceID   string `json:"deviceId"`
	DeviceType string `json:"deviceType"`
	DeviceName string `json:"deviceName"`
}

type SwitchBotResponse[T any] struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	Body       T      `json:"body"`
}

type SwitchBotDeviceListBody struct {
	DeviceList []Device `json:"deviceList"`
}

type SwitchBotDeviceStatus struct {
	Battery     int     `json:"battery"`
	Temperature float64 `json:"temperature"`
	Humidity    float64 `json:"humidity"`
	CO2         int     `json:"CO2"`
}

var AppConfig Config

func initConfig() error {
	if isLambda() {
		AppConfig = Config{
			SwitchBotToken:  os.Getenv("SWITCHBOT_API_TOKEN"),
			SwitchBotSecret: os.Getenv("SWITCHBOT_API_SECRET"),
			MastodonURL:     os.Getenv("MASTODON_API_URL"),
			MastodonToken:   os.Getenv("MASTODON_ACCESS_TOKEN"),
		}
	} else {
		file, err := os.Open("config.json")
		if err != nil {
			return err
		}
		defer file.Close()

		decoder := json.NewDecoder(file)
		if err := decoder.Decode(&AppConfig); err != nil {
			return err
		}
	}
	return nil
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}

func main() {
	if isLambda() {
		lambda.Start(handler)
	} else {
		if err := handler(context.Background()); err != nil {
			fmt.Println("Error:", err)
		}
	}
}

func handler(ctx context.Context) error {
	initConfig()

	statusMessages := []string{}
	devices, err := fetchDevices()
	if err != nil {
		return fmt.Errorf("fetchDevices error: %w", err)
	}

	targetDeviceTypes := map[string]bool{"MeterPro(CO2)": true, "Meter": true}
	for _, d := range devices {
		if targetDeviceTypes[d.DeviceType] {
			if msg, err := generateStatusMessage(d); err == nil {
				statusMessages = append(statusMessages, msg)
			}
		}
	}

	if len(statusMessages) > 0 {
		if err := postToMastodon(strings.Join(statusMessages, "\n")); err != nil {
			return fmt.Errorf("postToMastodon error: %w", err)
		}
	}

	return nil
}

func fetchDevices() ([]Device, error) {
	url := "https://api.switch-bot.com/v1.1/devices"
	var resp SwitchBotResponse[SwitchBotDeviceListBody]
	if err := requestWithBackoff(url, generateSwitchBotHeaders(), &resp); err != nil {
		return nil, err
	}
	return resp.Body.DeviceList, nil
}

func fetchDeviceStatus(device Device) (SwitchBotDeviceStatus, error) {
	url := fmt.Sprintf("https://api.switch-bot.com/v1.1/devices/%s/status", device.DeviceID)
	var resp SwitchBotResponse[SwitchBotDeviceStatus]
	if err := requestWithBackoff(url, generateSwitchBotHeaders(), &resp); err != nil {
		return SwitchBotDeviceStatus{}, err
	}
	return resp.Body, nil
}

func requestWithBackoff[T any](url string, headers map[string]string, out *SwitchBotResponse[T]) error {
	for attempt := 0; attempt < 5; attempt++ {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("request creation failed: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("HTTP request failed: %w", err)
		}
		defer res.Body.Close()

		bodyBytes, err := io.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("reading response failed: %w", err)
		}

		if err := json.Unmarshal(bodyBytes, out); err != nil {
			return fmt.Errorf("unmarshal failed: %w\nResponse body: %s", err, string(bodyBytes))
		}

		switch out.StatusCode {
		case 100:
			return nil
		case 190:
			if attempt < 4 {
				wait := time.Duration(1<<attempt) * time.Second
				fmt.Printf("[Retry %d/5] statusCode 190 received. Retrying after %v...\n", attempt+1, wait)
				time.Sleep(wait)
				continue
			}
			return fmt.Errorf("max retries reached for statusCode 190: %s", out.Message)
		default:
			return fmt.Errorf("unexpected statusCode %d: %s", out.StatusCode, out.Message)
		}
	}
	return fmt.Errorf("unreachable: requestWithBackoff fell through")
}

func generateSwitchBotHeaders() map[string]string {
	nonce := uuid.New().String()
	timestamp := strconv.FormatInt(time.Now().UnixNano()/1e6, 10)
	message := AppConfig.SwitchBotToken + timestamp + nonce

	h := hmac.New(sha256.New, []byte(AppConfig.SwitchBotSecret))
	h.Write([]byte(message))
	sign := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return map[string]string{
		"Authorization": AppConfig.SwitchBotToken,
		"Content-Type":  "application/json",
		"charset":       "utf8",
		"t":             timestamp,
		"sign":          sign,
		"nonce":         nonce,
	}
}

func generateStatusMessage(device Device) (string, error) {
	status, err := fetchDeviceStatus(device)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf(`# %s (🔋%d%%)
温度: %.1f度
湿度: %.1f%%
`,
		device.DeviceName,
		status.Battery,
		status.Temperature,
		status.Humidity,
	)
	if status.CO2 > 0 {
		msg += fmt.Sprintf("CO2: %dppm\n", status.CO2)
	}
	fmt.Println("Generated status message:", msg)
	return msg, nil
}

func postToMastodon(message string) error {
	url := AppConfig.MastodonURL + "/statuses"
	payload := map[string]string{
		"status":     message,
		"visibility": "unlisted",
	}
	buf, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(buf))
	req.Header.Set("Authorization", "Bearer "+AppConfig.MastodonToken)
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("Mastodon API error: %s", body)
	}
	fmt.Println("Post successful:", message)
	return nil
}
