// telegram bot for using raspberry pi camera module
package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	bot "github.com/meinside/telegram-bot-go"

	"github.com/meinside/telegram-bot-rpi-camera/conf"
	"github.com/meinside/telegram-bot-rpi-camera/helper"

	"github.com/meinside/loggly-go"
)

type status int16

// constants
const (
	statusWaiting status = iota

	numQueue        = 4
	numLatestPhotos = 20
)

// session struct
type _session struct {
	UserID        string
	CurrentStatus status
	LastUpdateID  int
}

// session pool for storing individual statuses
type _sessionPool struct {
	Sessions map[string]_session
	sync.Mutex
}

// for making sure the camera is not used simultaneously
var cameraLock sync.Mutex

// capture request
type _captureRequest struct {
	UserName       string
	ChatID         interface{}
	ImageWidth     int
	ImageHeight    int
	CameraParams   map[string]interface{}
	MessageOptions map[string]interface{}
}

// variables
var apiToken string
var monitorInterval int
var isVerbose bool
var availableIds []string
var imageWidth, imageHeight int
var cameraParams map[string]interface{}
var isInMaintenance bool
var maintenanceMessage string
var pool _sessionPool
var captureChannel chan _captureRequest
var launched time.Time
var logger *loggly.Loggly
var db *helper.Database

const (
	appName = "RPiCameraBot"
)

type logglyLog struct {
	Application string      `json:"app"`
	Severity    string      `json:"severity"`
	Timestamp   string      `json:"timestamp"`
	Message     string      `json:"message,omitempty"`
	Object      interface{} `json:"obj,omitempty"`
}

// keyboards
var allKeyboards = [][]bot.KeyboardButton{
	bot.NewKeyboardButtons(conf.CommandCapture),
	bot.NewKeyboardButtons(conf.CommandStatus, conf.CommandHelp),
}
var cancelKeyboard = [][]bot.KeyboardButton{
	bot.NewKeyboardButtons(conf.CommandCancel),
}

// initialization
func init() {
	launched = time.Now()

	// read variables from config file
	if config, err := helper.GetConfig(); err == nil {
		apiToken = config.ApiToken
		availableIds = config.AvailableIds
		monitorInterval = config.MonitorInterval
		if monitorInterval <= 0 {
			monitorInterval = conf.DefaultMonitorIntervalSeconds
		}
		isVerbose = config.IsVerbose

		// image width * height
		imageWidth = config.ImageWidth
		if imageWidth < conf.MinImageWidth {
			imageWidth = conf.MinImageWidth
		}
		imageHeight = config.ImageHeight
		if imageHeight < conf.MinImageHeight {
			imageHeight = conf.MinImageHeight
		}

		// other camera params
		cameraParams = config.CameraParams

		// maintenance
		isInMaintenance = config.IsInMaintenance
		maintenanceMessage = config.MaintenanceMessage
		if len(maintenanceMessage) <= 0 {
			maintenanceMessage = conf.DefaultMaintenanceMessage
		}

		// initialize session variables
		sessions := make(map[string]_session)
		for _, v := range availableIds {
			sessions[v] = _session{
				UserID:        v,
				CurrentStatus: statusWaiting,
				LastUpdateID:  -1,
			}
		}
		pool = _sessionPool{
			Sessions: sessions,
		}

		// channels
		captureChannel = make(chan _captureRequest, numQueue)

		// loggly
		if config.LogglyToken != "" {
			logger = loggly.New(config.LogglyToken)
		} else {
			logger = nil
		}

		// local database
		db = helper.OpenDb()
	} else {
		panic(err.Error())
	}
}

// check if given Telegram id is available
func isAvailableID(id string) bool {
	for _, v := range availableIds {
		if v == id {
			return true
		}
	}
	return false
}

// for showing help message
func getHelp() string {
	return fmt.Sprintf(`
Following commands are supported:

*For Raspberry Pi Camera Module*

%s : capture a still image with *raspistill*

*Others*

%s : show this bot's status
%s : show this help message

https://github.com/meinside/telegram-bot-rpi-camera
`,
		conf.CommandCapture,
		conf.CommandStatus,
		conf.CommandHelp,
	)
}

// for showing current status of this bot
func getStatus() string {
	return fmt.Sprintf("Uptime: %s\nMemory Usage: %s", helper.GetUptime(launched), helper.GetMemoryUsage())
}

// process incoming update from Telegram
func processUpdate(b *bot.Bot, update bot.Update) bool {
	// check username
	var userID string
	if update.Message.From.Username == nil {
		logError(fmt.Sprintf("Message - User not allowed (has no username): %s", update.Message.From.FirstName))
		return false
	}
	userID = *update.Message.From.Username
	if !isAvailableID(userID) {
		logError(fmt.Sprintf("Message - Id not allowed: %s", userID))
		return false
	}

	// process result
	result := false

	pool.Lock()
	if session, exists := pool.Sessions[userID]; exists {
		// XXX - for skipping duplicated update
		// (sometimes same update is retrieved again and again due to Telegram's API error)
		if session.LastUpdateID != update.UpdateID {
			// save last update id
			pool.Sessions[userID] = _session{
				UserID:        session.UserID,
				CurrentStatus: session.CurrentStatus,
				LastUpdateID:  update.UpdateID,
			}

			// text from message
			var txt string
			if update.Message.HasText() {
				txt = *update.Message.Text
			} else {
				txt = ""
			}

			var message, cmd string
			var options = map[string]interface{}{
				"reply_markup": bot.ReplyKeyboardMarkup{
					Keyboard:       allKeyboards,
					ResizeKeyboard: true,
				},
				"parse_mode": bot.ParseModeMarkdown,
			}

			switch session.CurrentStatus {
			case statusWaiting:
				switch {
				// start
				case strings.HasPrefix(txt, conf.CommandStart):
					message = conf.MessageDefault
					cmd = conf.CommandStart
				// capture
				case strings.HasPrefix(txt, conf.CommandCapture):
					message = ""
					cmd = conf.CommandCapture
				// status
				case strings.HasPrefix(txt, conf.CommandStatus):
					message = getStatus()
					cmd = conf.CommandStatus
				// help
				case strings.HasPrefix(txt, conf.CommandHelp):
					message = getHelp()
					cmd = conf.CommandHelp
				// fallback
				default:
					if len(txt) > 0 {
						message = fmt.Sprintf("*%s*: %s", txt, conf.MessageUnknownCommand)
					} else {
						message = conf.MessageUnknownCommand
					}
					cmd = "unknown"
				}
			}

			// log request
			logRequest(userID, cmd)

			if len(message) > 0 {
				// 'typing...'
				b.SendChatAction(update.Message.Chat.ID, bot.ChatActionTyping)

				// send message
				if sent := b.SendMessage(update.Message.Chat.ID, message, options); sent.Ok {
					result = true
				} else {
					logError(fmt.Sprintf("Failed to send message: %s", *sent.Description))
				}
			} else {
				if isInMaintenance {
					// send message
					if sent := b.SendMessage(update.Message.Chat.ID, maintenanceMessage, options); sent.Ok {
						result = true
					} else {
						logError(fmt.Sprintf("Failed to send maintenance message: %s", *sent.Description))
					}
				} else {
					// push to capture request channel
					captureChannel <- _captureRequest{
						UserName:       *update.Message.From.Username,
						ChatID:         update.Message.Chat.ID,
						ImageWidth:     imageWidth,
						ImageHeight:    imageHeight,
						CameraParams:   cameraParams,
						MessageOptions: options,
					}
				}
			}
		} else {
			logError(fmt.Sprintf("Duplicated update id: %d", update.UpdateID))
		}
	} else {
		logError(fmt.Sprintf("Session does not exist for id: %s", userID))
	}
	pool.Unlock()

	return result
}

// process capture request
func processCaptureRequest(b *bot.Bot, request _captureRequest) bool {
	// process result
	result := false

	cameraLock.Lock()
	defer cameraLock.Unlock()

	// 'typing...'
	b.SendChatAction(request.ChatID, bot.ChatActionTyping)

	// send photo
	if bytes, err := helper.CaptureRaspiStill(request.ImageWidth, request.ImageHeight, request.CameraParams); err == nil {
		// captured time
		caption := time.Now().Format("2006-01-02 (Mon) 15:04:05")
		request.MessageOptions["caption"] = caption

		// 'uploading photo...'
		b.SendChatAction(request.ChatID, bot.ChatActionUploadPhoto)

		// send photo
		if sent := b.SendPhoto(request.ChatID, bot.InputFileFromBytes(bytes), request.MessageOptions); sent.Ok {
			photo := sent.Result.LargestPhoto()

			db.SavePhoto(request.UserName, photo.FileID, caption)

			result = true
		} else {
			logError(fmt.Sprintf("Failed to send photo: %s", *sent.Description))
		}
	} else {
		message := fmt.Sprintf("Image capture failed: %s", err)

		logError(message)

		b.SendMessage(request.ChatID, message, request.MessageOptions)
	}

	return result
}

// process inline query
func processInlineQuery(b *bot.Bot, update bot.Update) bool {
	// check username
	var userID string
	if update.InlineQuery.From.Username == nil {
		logError(fmt.Sprintf("Inline Query - user not allowed (has no username): %s", update.Message.From.FirstName))
		return false
	}
	userID = *update.InlineQuery.From.Username
	if !isAvailableID(userID) {
		logError(fmt.Sprintf("Inline Query - id not allowed: %s", userID))
		return false
	}

	// retrieve cached photos,
	photos := db.GetPhotos(userID, numLatestPhotos)

	if len(photos) > 0 {
		photoResults := []interface{}{}

		// build up inline query results with cached photos,
		for _, photo := range photos {
			caption := photo.Caption

			if newPhoto, id := bot.NewInlineQueryResultCachedPhoto(photo.FileId); id != nil {
				newPhoto.Caption = &caption

				photoResults = append(photoResults, newPhoto)
			}
		}

		// then answer inline query
		sent := b.AnswerInlineQuery(
			update.InlineQuery.ID,
			photoResults,
			nil,
		)

		if sent.Ok {
			return true
		}

		logError(fmt.Sprintf("Failed to answer inline query: %s", *sent.Description))
	} else {
		logError("No cached photos for inline query.")
	}

	return false
}

func main() {
	client := bot.NewClient(apiToken)
	client.Verbose = isVerbose

	// get info about this bot
	if me := client.GetMe(); me.Ok {
		logMessage(fmt.Sprintf("Starting bot: @%s (%s)\n", *me.Result.Username, me.Result.FirstName))

		// delete webhook (getting updates will not work when wehbook is set up)
		if unhooked := client.DeleteWebhook(); unhooked.Ok {
			// monitor request capture channel
			go func() {
				for {
					select {
					case request := <-captureChannel:
						// do capture and send response
						processCaptureRequest(client, request)
					}
				}
			}()

			// wait for new updates
			client.StartMonitoringUpdates(0, monitorInterval, func(b *bot.Bot, update bot.Update, err error) {
				if err == nil {
					if update.HasMessage() {
						processUpdate(b, update)
					} else if update.HasInlineQuery() {
						processInlineQuery(b, update)
					}
				} else {
					logError(fmt.Sprintf("Error while receiving update (%s)", err.Error()))
				}
			})
		} else {
			panic("Failed to delete webhook")
		}
	} else {
		panic("Failed to get info of the bot")
	}
}

func logMessage(message string) {
	log.Println(message)

	if logger != nil {
		_, timestamp := loggly.Timestamp()

		logger.Log(logglyLog{
			Application: appName,
			Severity:    "Log",
			Timestamp:   timestamp,
			Message:     message,
		})
	}
}

func logError(message string) {
	log.Println(message)

	if logger != nil {
		_, timestamp := loggly.Timestamp()

		logger.Log(logglyLog{
			Application: appName,
			Severity:    "Error",
			Timestamp:   timestamp,
			Message:     message,
		})
	}
}

func logRequest(username, cmd string) {
	if logger != nil {
		_, timestamp := loggly.Timestamp()

		logger.Log(logglyLog{
			Application: appName,
			Severity:    "Verbose",
			Timestamp:   timestamp,
			Object: struct {
				Username string `json:"username"`
				Command  string `json:"command"`
			}{
				Username: username,
				Command:  cmd,
			},
		})
	}
}
