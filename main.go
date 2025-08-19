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
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/google/uuid"
)

var (
	batteryCheckPostCount = 7
	config                = Config{}
	htmlTagRe             = regexp.MustCompile(`<.*?>`)
	targetDeviceTypes     = map[string]struct{}{
		"Meter":         {},
		"MeterPro(CO2)": {},
	}
)

type Config struct {
	SwitchBotToken        string
	SwitchBotSecret       string
	MastodonURL           string
	MastodonToken         string
	BatteryCheckPostCount int
}

type SwitchBotDevice struct {
	DeviceID   string `json:"deviceId"`
	DeviceType string `json:"deviceType"`
	DeviceName string `json:"deviceName"`
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

type SwitchBotResponse[T any] struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	Body       T      `json:"body"`
}

type MastodonPost struct {
	Content string `json:"content"`
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
	if err := loadConfig(); err != nil {
		return fmt.Errorf("loadConfig error: %w", err)
	}

	devices, err := fetchDevices()
	if err != nil {
		return fmt.Errorf("fetchDevices error: %w", err)
	}

	posts, err := fetchRecentMastodonPosts()
	if err != nil {
		return fmt.Errorf("fetchRecentMastodonPosts error: %w", err)
	}

	var messages []string
	for _, device := range devices {
		if !isTargetDevice(device.DeviceType) {
			continue
		}
		message, err := generateStatusMessage(ctx, device, posts)
		if err != nil {
			continue
		}
		log.Println("Generated status message:", message)
		messages = append(messages, message)
	}

	if len(messages) > 0 {
		return postToMastodon(strings.Join(messages, "\n"))
	}
	return nil
}

func loadConfig() error {
	if isLambda() {
		if envPostCount := os.Getenv("BATTERY_CHECK_POST_COUNT"); envPostCount != "" {
			if count, err := strconv.Atoi(envPostCount); err == nil && count > 0 {
				batteryCheckPostCount = count
			}
		}
		config = Config{
			SwitchBotToken:        os.Getenv("SWITCHBOT_API_TOKEN"),
			SwitchBotSecret:       os.Getenv("SWITCHBOT_API_SECRET"),
			MastodonURL:           os.Getenv("MASTODON_API_URL"),
			MastodonToken:         os.Getenv("MASTODON_ACCESS_TOKEN"),
			BatteryCheckPostCount: batteryCheckPostCount,
		}
		return nil
	}
	file, err := os.Open("config.json")
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(&config)
}

func fetchDevices() ([]SwitchBotDevice, error) {
	url := "https://api.switch-bot.com/v1.1/devices"
	var resp SwitchBotResponse[SwitchBotDeviceListBody]
	if err := requestWithBackoff(url, generateSwitchBotHeaders(), &resp); err != nil {
		return nil, err
	}
	return resp.Body.DeviceList, nil
}

func requestWithBackoff[T any](url string, headers map[string]string, out *SwitchBotResponse[T]) error {
	for attempt := range 5 {
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
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	message := config.SwitchBotToken + timestamp + nonce

	h := hmac.New(sha256.New, []byte(config.SwitchBotSecret))
	h.Write([]byte(message))
	sign := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return map[string]string{
		"Authorization": config.SwitchBotToken,
		"Content-Type":  "application/json",
		"charset":       "utf8",
		"t":             timestamp,
		"sign":          sign,
		"nonce":         nonce,
	}
}

func fetchRecentMastodonPosts() ([]MastodonPost, error) {
	var verifyResp struct {
		ID string `json:"id"`
	}
	if err := httpGet("/accounts/verify_credentials", &verifyResp); err != nil {
		return nil, err
	}

	var posts []MastodonPost
	if err := httpGet(fmt.Sprintf("/accounts/%s/statuses?limit=%d", verifyResp.ID, batteryCheckPostCount), &posts); err != nil {
		return nil, err
	}
	return posts, nil
}

func httpGet(endpoint string, result any) error {
	url := config.MastodonURL + endpoint
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+config.MastodonToken)
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

func isTargetDevice(deviceType string) bool {
	_, ok := targetDeviceTypes[deviceType]
	return ok
}

func generateStatusMessage(ctx context.Context, device SwitchBotDevice, posts []MastodonPost) (string, error) {
	status, err := fetchDeviceStatus(device)
	if err != nil {
		return "", err
	}

	if err := PutMetric(ctx, device, status); err != nil {
		log.Printf("Failed to send metrics to CloudWatch: %v", err)
	}

	var b strings.Builder
	b.WriteString(makeDeviceHeader(device.DeviceName))
	if status.Battery != nil {
		emoji, err := batteryStatusEmoji(device, posts, status)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, " (%s%d%%)", emoji, *status.Battery)
	}
	b.WriteByte('\n')
	if status.Temperature != nil {
		fmt.Fprintf(&b, "温度: %.1f度\n", *status.Temperature)
	}
	if status.Humidity != nil {
		fmt.Fprintf(&b, "湿度: %.1f%%\n", *status.Humidity)
	}
	if status.CO2 != nil {
		var icon string
		switch {
		case *status.CO2 >= 1500:
			icon = "🔥"
		case *status.CO2 >= 1000:
			icon = "💨"
		default:
			icon = "🌳"
		}
		fmt.Fprintf(&b, "CO2: %dppm %s\n", *status.CO2, icon)
	}
	return b.String(), nil
}

func fetchDeviceStatus(device SwitchBotDevice) (SwitchBotDeviceStatus, error) {
	url := fmt.Sprintf("https://api.switch-bot.com/v1.1/devices/%s/status", device.DeviceID)
	var resp SwitchBotResponse[SwitchBotDeviceStatus]
	if err := requestWithBackoff(url, generateSwitchBotHeaders(), &resp); err != nil {
		return SwitchBotDeviceStatus{}, err
	}
	return resp.Body, nil
}

func PutMetric(ctx context.Context, device SwitchBotDevice, status SwitchBotDeviceStatus) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	cw := cloudwatch.NewFromConfig(cfg)

	timestamp := aws.Time(time.Now())

	var metricData []types.MetricDatum

	if status.Temperature != nil {
		metricData = append(metricData, types.MetricDatum{
			MetricName: aws.String("Temperature"),
			Dimensions: []types.Dimension{
				{Name: aws.String("DeviceId"), Value: aws.String(device.DeviceID)},
			},
			Timestamp: timestamp,
			Value:     aws.Float64(*status.Temperature),
			Unit:      types.StandardUnitCount,
		})
	}

	if status.Humidity != nil {
		metricData = append(metricData, types.MetricDatum{
			MetricName: aws.String("Humidity"),
			Dimensions: []types.Dimension{
				{Name: aws.String("DeviceId"), Value: aws.String(device.DeviceID)},
			},
			Timestamp: timestamp,
			Value:     aws.Float64(*status.Humidity),
			Unit:      types.StandardUnitPercent,
		})
	}

	if status.CO2 != nil {
		metricData = append(metricData, types.MetricDatum{
			MetricName: aws.String("CO2"),
			Dimensions: []types.Dimension{
				{Name: aws.String("DeviceId"), Value: aws.String(device.DeviceID)},
			},
			Timestamp: timestamp,
			Value:     aws.Float64(float64(*status.CO2)),
			Unit:      types.StandardUnitCount,
		})
	}

	if len(metricData) == 0 {
		return nil
	}

	_, err = cw.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace:  aws.String("SwitchBotMetrics"),
		MetricData: metricData,
	})
	if err != nil {
		return fmt.Errorf("failed to put metric data: %w", err)
	}

	return nil
}

func makeDeviceHeader(deviceName string) string {
	return fmt.Sprintf("# %s", deviceName)
}

func batteryStatusEmoji(device SwitchBotDevice, posts []MastodonPost, status SwitchBotDeviceStatus) (string, error) {
	previousMessages, err := extractRecentMessagesForDevice(device.DeviceName, posts)
	if err != nil {
		return "", fmt.Errorf("extractRecentMessagesForDevice failed: %w", err)
	}
	if isRepeated(status, previousMessages) {
		return "⚠️", nil
	}
	return "🔋", nil
}

func extractRecentMessagesForDevice(deviceName string, posts []MastodonPost) ([]string, error) {
	var messages []string
	for _, post := range posts {
		text := stripHTMLTags(post.Content)
		if idx := strings.Index(text, makeDeviceHeader(deviceName)); idx != -1 {
			messages = append(messages, text[idx:])
		}
	}
	return messages, nil
}

func stripHTMLTags(input string) string {
	return htmlTagRe.ReplaceAllString(input, "")
}

func isRepeated(current SwitchBotDeviceStatus, previousMessages []string) bool {
	for _, msg := range previousMessages {
		temp := extractFloatValue(msg, `温度: ([\d.]+)度`)
		hum := extractFloatValue(msg, `湿度: ([\d.]+)%`)
		co2 := extractIntValue(msg, `CO2: (\d+)ppm`)
		if !ptrEquals(temp, current.Temperature) ||
			!ptrEquals(hum, current.Humidity) ||
			!ptrEquals(co2, current.CO2) {
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

func ptrEquals[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func postToMastodon(message string) error {
	url := config.MastodonURL + "/statuses"
	payload := map[string]string{
		"status":     message,
		"visibility": "unlisted",
	}
	buf, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(buf))
	req.Header.Set("Authorization", "Bearer "+config.MastodonToken)
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("mastodon API error: %s", body)
	}
	log.Println("Post successful:", message)
	return nil
}
