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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/google/uuid"
)

const (
	batteryCheckPostCount = 5
)

var (
	AppConfig Config
	htmlTagRe = regexp.MustCompile(`<.*?>`)
)

type Config struct {
	SwitchBotToken  string
	SwitchBotSecret string
	MastodonURL     string
	MastodonToken   string
}

type SwitchBotDevice struct {
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
	DeviceList []SwitchBotDevice `json:"deviceList"`
}

type SwitchBotDeviceStatus struct {
	Battery     *int     `json:"battery,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	Humidity    *float64 `json:"humidity,omitempty"`
	CO2         *int     `json:"CO2,omitempty"`
}

func main() {
	if isLambda() {
		lambda.Start(handler)
	} else if err := handler(context.Background()); err != nil {
		fmt.Println("Error:", err)
	}
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}

func handler(ctx context.Context) error {
	if err := initConfig(); err != nil {
		return fmt.Errorf("initConfig error: %w", err)
	}

	devices, err := fetchDevices()
	if err != nil {
		return fmt.Errorf("fetchDevices error: %w", err)
	}

	var statusMessages []string
	for _, d := range devices {
		if isTargetDevice(d.DeviceType) {
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

func initConfig() error {
	if isLambda() {
		AppConfig = Config{
			SwitchBotToken:  os.Getenv("SWITCHBOT_API_TOKEN"),
			SwitchBotSecret: os.Getenv("SWITCHBOT_API_SECRET"),
			MastodonURL:     os.Getenv("MASTODON_API_URL"),
			MastodonToken:   os.Getenv("MASTODON_ACCESS_TOKEN"),
		}
		return nil
	}

	file, err := os.Open("config.json")
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(&AppConfig)
}

func fetchDevices() ([]SwitchBotDevice, error) {
	url := "https://api.switch-bot.com/v1.1/devices"
	var resp SwitchBotResponse[SwitchBotDeviceListBody]
	if err := requestWithBackoff(url, generateSwitchBotHeaders(), &resp); err != nil {
		return nil, err
	}
	return resp.Body.DeviceList, nil
}

func fetchDeviceStatus(device SwitchBotDevice) (SwitchBotDeviceStatus, error) {
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

func isTargetDevice(t string) bool {
	switch t {
	case "Meter", "MeterPro(CO2)":
		return true
	}
	return false
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

func generateStatusMessage(device SwitchBotDevice) (string, error) {
	status, err := fetchDeviceStatus(device)
	if err != nil {
		return "", err
	}

	msg := "# " + device.DeviceName
	if status.Battery != nil {
		emoji, err := getBatteryStatusSymbol(device, status)
		if err != nil {
			return "", err
		}
		msg += fmt.Sprintf(" (%s%d%%)\n", emoji, *status.Battery)
	}
	if status.Temperature != nil {
		msg += fmt.Sprintf("温度: %.1f度\n", *status.Temperature)
	}
	if status.Humidity != nil {
		msg += fmt.Sprintf("湿度: %.1f%%\n", *status.Humidity)
	}
	if status.CO2 != nil {
		msg += fmt.Sprintf("CO2: %dppm\n", *status.CO2)
	}
	fmt.Println("Generated status message:", msg)
	return msg, nil
}

func getBatteryStatusSymbol(device SwitchBotDevice, status SwitchBotDeviceStatus) (string, error) {
	posts, err := fetchLatestPosts(device.DeviceName)
	if err != nil {
		return "", fmt.Errorf("fetchLatestPosts failed: %w", err)
	}

	if isRepeated(status, posts) {
		return "⚠️", nil
	}
	return "🔋", nil
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

func fetchLatestPosts(deviceName string) ([]string, error) {
	var verifyResp struct {
		ID string `json:"id"`
	}
	if err := httpGet("/accounts/verify_credentials", &verifyResp); err != nil {
		return nil, err
	}

	var statuses []struct {
		Content string `json:"content"`
	}
	if err := httpGet(fmt.Sprintf("/accounts/%s/statuses?limit=%d", verifyResp.ID, batteryCheckPostCount), &statuses); err != nil {
		return nil, err
	}

	var posts []string
	for _, s := range statuses {
		text := stripHTMLTags(s.Content)
		if idx := strings.Index(text, "# "+deviceName); idx != -1 {
			posts = append(posts, text[idx:])
		}
	}
	return posts, nil
}

func httpGet(endpoint string, result any) error {
	url := AppConfig.MastodonURL + endpoint

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+AppConfig.MastodonToken)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("GET %s failed: %s", url, body)
	}

	return json.NewDecoder(res.Body).Decode(result)
}

func stripHTMLTags(input string) string {
	return htmlTagRe.ReplaceAllString(input, "")
}

func isRepeated(current SwitchBotDeviceStatus, previousPosts []string) bool {
	for _, post := range previousPosts {
		temp := extractFloatValue(post, `温度: ([\d.]+)度`)
		hum := extractFloatValue(post, `湿度: ([\d.]+)%`)
		co2 := extractIntValue(post, `CO2: (\d+)ppm`)

		if !floatPtrEquals(temp, current.Temperature) ||
			!floatPtrEquals(hum, current.Humidity) ||
			!intPtrEquals(co2, current.CO2) {
			return false
		}
	}
	return true
}

func extractFloatValue(text, pattern string) *float64 {
	matches := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if len(matches) < 2 {
		return nil
	}
	v, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return nil
	}
	return &v
}

func extractIntValue(text, pattern string) *int {
	matches := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if len(matches) < 2 {
		return nil
	}
	v, err := strconv.Atoi(matches[1])
	if err != nil {
		return nil
	}
	return &v
}

func floatPtrEquals(a, b *float64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func intPtrEquals(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
