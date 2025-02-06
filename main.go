package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/time/rate"
	"gopkg.in/gomail.v2"
)

var err error = godotenv.Load()
var emailDialer *gomail.Dialer
var rdb *redis.Client
var limiter = rate.NewLimiter(rate.Every(time.Second/10), 1)

const trackingPrefix = "tracking:"

// Cache parsed templates
var templateCache sync.Map

func main() {
	_, err := os.Stat(".env")
	if err == nil {
		err := godotenv.Load()
		if err != nil {
			log.Fatal("Error loading .env file")
		}
	}

	PORT := os.Getenv("PORT")

	if PORT == "" {
		PORT = "8080"
	}

	if err := initEmailDialer(); err != nil {
		log.Fatal("Failed to initialize email dialer: ", err)
	}

	if err := initRedisClient(); err != nil {
		log.Fatal("Failed to initialize Redis: ", err)
	}

	router := gin.Default()

	router.POST("/element", handleElementRequest)
	router.GET("/pixel/:tracking_id", handleTrackingPixel)
	router.GET("/status/:tracking_id", handleTrackingStatus)

	router.Run(":" + PORT)
}

func initEmailDialer() error {
	smtpPort, err := strconv.Atoi(os.Getenv("SMTP_PORT"))
	if err != nil {
		return err
	}

	emailDialer = gomail.NewDialer(
		os.Getenv("SMTP_HOST"),
		smtpPort,
		os.Getenv("SMTP_USERNAME"),
		os.Getenv("SMTP_PASSWORD"),
	)

	emailDialer.TLSConfig = &tls.Config{
		ServerName: os.Getenv("SMTP_HOST"),
	}

	return nil
}

func initRedisClient() error {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "redis:6379"
	}

	rdb = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: "", // no password set for now
		DB:       0,  // use default DB
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis connection failed: %w", err)
	}

	log.Printf("Connected to Redis at %s", redisAddr)
	return nil
}

type Receiver struct {
	Email       string `json:"email"`
	TrackingId  string `json:"tracking_id"`
	WantToTrack bool   `json:"want_to_track"`
	Type        string `json:"type"`
}

type Recipients struct {
	Receivers []Receiver `json:"receivers"`
	From      string     `json:"from"`
}

type EmailBody struct {
	HTMLTemplate string                 `json:"html_template"`
	Subject      string                 `json:"subject"`
	Parameters   map[string]interface{} `json:"parameters"`
}

type TrackingObject struct {
	Email      string    `json:"email"`
	Count      int       `json:"count"`
	LastOpened time.Time `json:"last_opened"`
}

type EmailTrackingRequest struct {
	Recipients Recipients `json:"recipients"`
	EmailBody  EmailBody  `json:"email_body"`
}

func handleElementRequest(c *gin.Context) {
	var emailTrackingRequest EmailTrackingRequest
	if err := c.BindJSON(&emailTrackingRequest); err != nil {
		c.JSON(400, gin.H{"error": "Invalid request"})
		return
	}

	if len(emailTrackingRequest.Recipients.Receivers) == 0 || emailTrackingRequest.EmailBody.HTMLTemplate == "" {
		c.JSON(400, gin.H{"error": "Missing recipients or email body"})
		return
	}

	statusMap := sendEmails(emailTrackingRequest.Recipients, emailTrackingRequest.EmailBody)

	c.JSON(200, gin.H{
		"status": statusMap,
	})
}

func handleTrackingPixel(c *gin.Context) {
	trackingId := c.Param("tracking_id")
	ctx := context.Background()

	key := trackingPrefix + trackingId
	var trackingObj TrackingObject
	data, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		c.Status(404)
		return
	}

	if err := json.Unmarshal(data, &trackingObj); err != nil {
		c.Status(500)
		return
	}

	trackingObj.Count++
	trackingObj.LastOpened = time.Now()

	updatedData, _ := json.Marshal(trackingObj)
	rdb.Set(ctx, key, updatedData, getTrackingExpiration())

	// Return transparent pixel
	pixelData, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
	)
	c.Data(200, "image/png", pixelData)
}
func handleTrackingStatus(c *gin.Context) {
	trackingId := c.Param("tracking_id")
	ctx := context.Background()

	key := trackingPrefix + trackingId
	var trackingObj TrackingObject
	data, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		c.JSON(404, gin.H{
			"error": "Tracking ID not found",
		})
		return
	}

	if err := json.Unmarshal(data, &trackingObj); err != nil {
		c.JSON(500, gin.H{
			"error": "Internal server error",
		})
		return
	}

	c.JSON(200, gin.H{
		"email":       trackingObj.Email,
		"count":       trackingObj.Count,
		"last_opened": trackingObj.LastOpened,
	})
}
func sendEmails(recipients Recipients, emailBody EmailBody) map[string]interface{} {
	statusMap := make(map[string]interface{})
	emailToTemplateMap := setHtml(emailBody)
	var wg sync.WaitGroup
	var mu sync.Mutex // Create a mutex to guard statusMap

	for _, receiver := range recipients.Receivers {
		wg.Add(1)
		go func(r Receiver) {
			defer wg.Done()
			result := processReceiver(r, recipients, emailBody, emailToTemplateMap)
			// Lock before writing to the map
			mu.Lock()
			statusMap[r.Email] = result
			mu.Unlock()
		}(receiver)
	}

	wg.Wait()
	return statusMap
}

func processReceiver(r Receiver, recipients Recipients, emailBody EmailBody, templates map[string]interface{}) string {
	// Generate tracking ID before creating message
	if r.WantToTrack {
		r.TrackingId = setTrackingId(r.Email, r.TrackingId, true)
	}

	m, err := createEmailMessage(r, recipients.From, emailBody.Subject, templates)
	if err != nil {
		return "Failed to create email"
	}

	if err := limiter.Wait(context.Background()); err != nil {
		return "Rate limit exceeded"
	}

	if err := emailDialer.DialAndSend(m); err != nil {
		return "Send failed"
	}

	return "Success:tracking_id:" + r.TrackingId
}

func createEmailMessage(r Receiver, from string, subject string, templates map[string]interface{}) (*gomail.Message, error) {
	m := gomail.NewMessage()
	m.SetHeader("From", from)
	m.SetHeader("Subject", subject)

	switch r.Type {
	case "to", "cc", "bcc":
		caser := cases.Title(language.English)
		m.SetHeader(caser.String(r.Type), r.Email)
	default:
		return nil, fmt.Errorf("invalid recipient type")
	}

	template, ok := templates[r.Email].(string)
	if !ok {
		return nil, fmt.Errorf("template not found")
	}

	if r.WantToTrack && r.TrackingId != "" {
		pixelHtml := fmt.Sprintf(`<img src="%s/pixel/%s" alt="" width="1" height="1" style="display:none"/>`,
			os.Getenv("TRACKING_DOMAIN"), r.TrackingId)
		template = strings.Replace(template, "</body>", pixelHtml+"</body>", 1)
	}

	m.SetBody("text/html", template)
	return m, nil
}

func setHtml(emailBody EmailBody) map[string]interface{} {
	if cached, ok := templateCache.Load(emailBody.HTMLTemplate); ok {
		return cached.(map[string]interface{})
	}
	originalTemplate := emailBody.HTMLTemplate
	emailToTemplateMap := make(map[string]interface{})

	for key, value := range emailBody.Parameters {
		email := key
		templ := originalTemplate
		for k, v := range value.(map[string]interface{}) {
			templ = strings.ReplaceAll(templ, "{{"+k+"}}", v.(string))
		}
		emailToTemplateMap[email] = templ
	}

	templateCache.Store(emailBody.HTMLTemplate, emailToTemplateMap)
	return emailToTemplateMap
}
func setTrackingId(email string, trackingId string, generateId bool) string {
	ctx := context.Background()
	if generateId {
		trackingId = uuid.New().String()
	}

	trackingObject := TrackingObject{
		Email:      email,
		Count:      0,
		LastOpened: time.Now(),
	}

	trackingObjectJson, _ := json.Marshal(trackingObject)
	key := trackingPrefix + trackingId
	rdb.Set(ctx, key, trackingObjectJson, getTrackingExpiration())

	return trackingId
}

func getTrackingExpiration() time.Duration {
	expirationStr := os.Getenv("TRACKING_ID_EXPIRATION")
	expiration, err := strconv.Atoi(expirationStr)
	if err != nil || expirationStr == "" {
		expiration = 7 * 24 * 60 * 60 // default to 7 day in seconds
	}
	return time.Duration(expiration) * time.Second
}
