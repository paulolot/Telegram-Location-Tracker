// Telegram Location Tracker Bot
// A PocketBase application that tracks user locations from Telegram and provides
// a web interface for viewing locations on a map and sending messages to users.
package main

import (
	"context"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/joho/godotenv"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/pocketbase/pocketbase/tools/template"
	"github.com/pocketbase/pocketbase/tools/types"
)

// Global instances - shared between HTTP handlers and Telegram bot
var app *pocketbase.PocketBase
var tgBot *bot.Bot

// SendMessageRequest represents the API request structure for sending messages to Telegram users
type SendMessageRequest struct {
	UserId string `json:"user_id" binding:"required"`
	Text   string `json:"text" binding:"required"`
}

func main() {
	// Load environment variables (BOT_TOKEN, etc.)
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}

	app = pocketbase.New()

	// Enable auto-migration only in development (when running with 'go run')
	// This allows PocketBase to automatically create migration files when collections change
	isGoRun := strings.HasPrefix(os.Args[0], os.TempDir())

	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: isGoRun,
	})

	// Setup HTTP routes and start Telegram bot when PocketBase server starts
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		_, err := e.App.FindCollectionByNameOrId("locations")
		if err != nil {
			collection := core.NewBaseCollection("locations")

			collection.ListRule = types.Pointer("")
			collection.ViewRule = types.Pointer("")

			collection.Fields.Add(
				&core.TextField{
					Name:     "user_id",
					Required: true,
				},
				&core.TextField{
					Name:     "user_name",
					Required: true,
				},
				&core.GeoPointField{
					Name: "location",
				},
			)

			err := app.Save(collection)
			if err != nil {
				return err
			}
		}

		// Start Telegram bot in a separate goroutine
		go startTelegramBot(e)

		// Load and render the main HTML template
		html, err := template.NewRegistry().LoadFiles(
			"index.html",
		).Render(nil)

		if err != nil {
			return err
		}

		// Serve the main web interface
		e.Router.GET("/", func(e *core.RequestEvent) error {
			return e.HTML(http.StatusOK, html)
		})

		// API endpoint to receive location from iVan bot (Google Apps Script)
		e.Router.POST("/api/location", func(e *core.RequestEvent) error {
			var payload struct {
				VehicleID string  `json:"vehicle_id"`
				Lat       float64 `json:"lat"`
				Lng       float64 `json:"lng"`
				Timestamp int64   `json:"timestamp"`
				Token     string  `json:"token"`
			}

			if err := e.BindBody(&payload); err != nil {
				return e.BadRequestError("Invalid JSON payload", err)
			}

			// Simple secret token check
			secretToken := os.Getenv("SECRET_TOKEN")
			if secretToken != "" && payload.Token != secretToken {
				return e.UnauthorizedError()
			}

			if payload.VehicleID == "" {
				return e.BadRequestError("vehicle_id is required", nil)
			}

			// Create a location record in PocketBase
			collection, err := e.App.FindCollectionByNameOrId("locations")
			if err != nil {
				return e.InternalServerError("Locations collection not found", err)
			}

			record := core.NewRecord(collection)
			record.Set("user_id", payload.VehicleID)
			record.Set("user_name", "Van - "+payload.VehicleID) // Could be dynamic
			
			// GeoPoint structure expected by PocketBase GeoPointField
			// For pocketbase v0.22+ it uses types.GeoPoint or simply unmarshals it
			// However, setting the field correctly via map might be easier.
			// Let's use the appropriate method.
			
			// Try to set location field
			record.Set("location", map[string]float64{
				"lat": payload.Lat,
				"lon": payload.Lng,
			})

			if err := e.App.Save(record); err != nil {
				log.Printf("Failed to save location record: %v", err)
				return e.InternalServerError("Failed to save location", err)
			}

			return e.JSON(200, map[string]string{
				"status": "success",
			})
		}).Bind()

		// API endpoint for sending messages to Telegram users from the web interface
		e.Router.POST("/api/sendMessage", func(e *core.RequestEvent) error {
			var request SendMessageRequest
			if err := e.BindBody(&request); err != nil {
				return e.BadRequestError("Invalid request body", err)
			}

			if request.UserId == "" || request.Text == "" {
				return e.BadRequestError("user_id and text are required", nil)
			}

			if tgBot == nil {
				return e.InternalServerError("Telegram bot not initialized", nil)
			}

			log.Printf("Sending message to user %s: %s", request.UserId, request.Text)

			_, err := tgBot.SendMessage(context.TODO(), &bot.SendMessageParams{
				ChatID: request.UserId,
				Text:   request.Text,
			})

			if err != nil {
				log.Printf("Failed to send message to user %s: %v", request.UserId, err)
				return e.InternalServerError("Could not send message", err)
			}

			log.Printf("Message sent successfully to user %s", request.UserId)
			return e.JSON(200, map[string]any{
				"message": "Message sent successfully",
				"user_id": request.UserId,
			})
		}).Bind()

		return e.Next()
	})

	// To handle the persistent volume in Railway, we check for an environment variable
	// PB_DATA_DIR, or if running in Railway, default to "/railway/static"
	dataDir := os.Getenv("PB_DATA_DIR")
	if dataDir == "" {
		if os.Getenv("RAILWAY_ENVIRONMENT") != "" {
			dataDir = "/railway/static/pb_data"
		} else {
			dataDir = "./pb_data" // local fallback
		}
	}
	
	app.SetDataDir(dataDir)

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// startTelegramBot initializes and starts the Telegram bot with graceful shutdown
func startTelegramBot(e *core.ServeEvent) {
	// Setup context that cancels on interrupt signal for graceful shutdown
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)

	opts := []bot.Option{
		bot.WithDefaultHandler(handler),
	}

	var err error
	tgBot, err = bot.New(os.Getenv("BOT_TOKEN"), opts...)
	if err != nil {
		log.Printf("Failed to create Telegram bot: %v", err)
		return
	}

	log.Println("Telegram bot started successfully")
	tgBot.Start(ctx)
}

// handler processes incoming Telegram messages, specifically location updates
// It implements distance-based filtering to avoid storing redundant location data
func handler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if app == nil {
		return
	}

	// Only process location messages from valid users
	if update == nil || update.Message == nil || update.Message.From == nil || update.Message.Location == nil {
		return
	}

	collection, err := app.FindCollectionByNameOrId("locations")
	if err != nil {
		log.Println(err)
		return
	}

	userId := update.Message.From.ID

	// Get the most recent location for this user to check distance
	records, err := app.FindRecordsByFilter(
		"locations",
		"user_id = {:user_id}",
		"-created",
		1,
		0,
	)
	if err != nil {
		log.Println(err)
	}

	// Distance-based filtering: only save location if user moved more than 10 meters
	// This prevents database spam from GPS jitter while stationary
	if len(records) > 0 {
		prevLoc := records[0].GetGeoPoint("location")
		newLat := update.Message.Location.Latitude
		newLon := update.Message.Location.Longitude
		prevLat := prevLoc.Lat
		prevLon := prevLoc.Lon

		// Haversine formula for calculating distance between two GPS coordinates
		const earthRadius = 6371000.0 // Earth's radius in meters
		toRad := func(deg float64) float64 { return deg * (3.141592653589793 / 180.0) }
		dLat := toRad(newLat - prevLat)
		dLon := toRad(newLon - prevLon)
		lat1 := toRad(prevLat)
		lat2 := toRad(newLat)

		a := (math.Sin(dLat/2) * math.Sin(dLat/2)) +
			(math.Cos(lat1) * math.Cos(lat2) * math.Sin(dLon/2) * math.Sin(dLon/2))
		c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
		distance := earthRadius * c

		// Skip saving if movement is less than 10 meters (likely GPS noise)
		if distance < 10.0 {
			return
		}
	}

	// Create and save new location record
	record := core.NewRecord(collection)
	record.Set("user_id", userId)
	record.Set("user_name", update.Message.From.FirstName)
	record.Set("location", types.GeoPoint{
		Lon: update.Message.Location.Longitude,
		Lat: update.Message.Location.Latitude,
	})
	_ = app.Save(record)
}
