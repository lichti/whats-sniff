package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var cli *whatsmeow.Client
var log waLog.Logger

var logLevel = "INFO"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var dbAddress = flag.String("db-address", "file:ws_data/db/whatsapp.db?_foreign_keys=on", "Database address")
var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")
var mediaPath = flag.String("media-path", "ws_data/media", "Path to store media files in")
var historyPath = flag.String("history-path", "ws_data/history", "Path to store history files in")
var pocketbaseURL = flag.String("pocketbase-url", "http://pocketbase:8090", "URL to Pocketbase instance")

var pairRejectChan = make(chan bool, 1)

func main() {

	waBinary.IndentXML = true
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
	}
	log = waLog.Stdout("Main", logLevel, true)

	dbLog := waLog.Stdout("Database", logLevel, true)
	storeContainer, err := sqlstore.New(*dbDialect, *dbAddress, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to database: %v", err)
		return
	}
	device, err := storeContainer.GetFirstDevice()
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	cli = whatsmeow.NewClient(device, waLog.Stdout("Client", logLevel, true))
	var isWaitingForPair atomic.Bool
	cli.PrePairCallback = func(jid types.JID, platform, businessName string) bool {
		isWaitingForPair.Store(true)
		defer isWaitingForPair.Store(false)
		log.Infof("Pairing %s (platform: %q, business name: %q). Type r within 3 seconds to reject pair", jid, platform, businessName)
		select {
		case reject := <-pairRejectChan:
			if reject {
				log.Infof("Rejecting pair")
				return false
			}
		case <-time.After(3 * time.Second):
		}
		log.Infof("Accepting pair")
		return true
	}

	ch, err := cli.GetQRChannel(context.Background())
	if err != nil {
		// This error means that we're already logged in, so ignore it.
		if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			log.Errorf("Failed to get QR channel: %v", err)
		}
	} else {
		go func() {
			for evt := range ch {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				} else {
					log.Infof("QR channel result: %s", evt.Event)
				}
			}
		}()
	}

	cli.AddEventHandler(handler)
	err = cli.Connect()
	if err != nil {
		log.Errorf("Failed to connect: %v", err)
		return
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	for {
		select {
		case <-c:
			log.Infof("Interrupt received, exiting")
			cli.Disconnect()
			return
		}
	}
}

var historySyncID int32
var startupTime = time.Now().Unix()

func handler(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		if len(cli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := cli.SendPresence(types.PresenceAvailable)
			if err != nil {
				postError("AppStateSyncComplete", "Failed to send presence", rawEvt)
			} else {
				postEvent("AppStateSyncComplete", rawEvt, nil)
			}
		}
		return
	case *events.Connected, *events.PushNameSetting:
		if len(cli.Store.PushName) == 0 {
			postEvent("Connected", rawEvt, nil)
			return
		}
		err := cli.SendPresence(types.PresenceAvailable)
		if err != nil {
			postError("Connected", "Failed to send presence", rawEvt)
		} else {
			postEvent("Connected", rawEvt, nil)
		}
		return
	case *events.StreamReplaced:
		postError("StreamReplaced", "Stream replaced", rawEvt)
		os.Exit(0)
	case *events.Message:
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		if evt.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", evt.Info.Type))
		}
		if evt.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", evt.Info.Category))
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "ephemeral")
		}
		if evt.IsViewOnceV2 {
			metaParts = append(metaParts, "ephemeral (v2)")
		}
		if evt.IsDocumentWithCaption {
			metaParts = append(metaParts, "document with caption")
		}
		if evt.IsEdit {
			metaParts = append(metaParts, "edit")
		}

		log.Infof("Received message %s from %s (%s)", evt.Info.ID, evt.Info.SourceString(), strings.Join(metaParts, ", "))

		if evt.Message.GetPollUpdateMessage() != nil {
			decrypted, err := cli.DecryptPollVote(evt)
			if err != nil {
				postError("Message.GetPollUpdateMessage", "Failed to decrypt vote", rawEvt)
			} else {
				postEvent("Message.GetPollUpdateMessage", rawEvt, decrypted)
			}
			return
		} else if evt.Message.GetEncReactionMessage() != nil {
			decrypted, err := cli.DecryptReaction(evt)
			if err != nil {
				postError("Message.GetEncReactionMessage", "Failed to decrypt encrypted reaction", rawEvt)
			} else {
				postEvent("Message.GetEncReactionMessage", rawEvt, decrypted)
			}
			return
		}

		img := evt.Message.GetImageMessage()
		if img != nil {
			data, err := cli.Download(img)
			if err != nil {
				postError("Message.GetImageMessage", "Failed to download image", rawEvt)
				return
			}
			exts, _ := mime.ExtensionsByType(img.GetMimetype())
			file_name := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			err = postEventFile("Message.GetImageMessage", rawEvt, nil, file_name, data)
			if err != nil {
				postError("Message.GetImageMessage", "Failed to save image", rawEvt)
				return
			}
			return
		}

		audio := evt.Message.GetAudioMessage()
		if audio != nil {
			data, err := cli.Download(audio)
			if err != nil {
				postError("Message.GetAudioMessage", "Failed to download audio", rawEvt)
				return
			}
			exts, _ := mime.ExtensionsByType(audio.GetMimetype())
			file_name := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			err = postEventFile("Message.GetAudioMessage", rawEvt, nil, file_name, data)
			if err != nil {
				postError("Message.GetAudioMessage", "Failed to save audio", rawEvt)
				return
			}
			return
		}

		video := evt.Message.GetVideoMessage()
		if video != nil {
			data, err := cli.Download(video)
			if err != nil {
				postError("Message.GetVideoMessage", "Failed to download video", rawEvt)
				return
			}
			exts, _ := mime.ExtensionsByType(video.GetMimetype())
			file_name := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			err = postEventFile("Message.GetVideoMessage", rawEvt, nil, file_name, data)
			if err != nil {
				log.Errorf("Failed to save video: %v", err)
				postError("Message.GetVideoMessage", "Failed to save video", rawEvt)
				return
			}
			return
		}

		doc := evt.Message.GetDocumentMessage()
		if doc != nil {
			data, err := cli.Download(doc)
			if err != nil {
				postError("Message.GetDocumentMessage", "Failed to download document", rawEvt)
				return
			}
			exts, _ := mime.ExtensionsByType(doc.GetMimetype())
			file_name := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			err = postEventFile("Message.GetDocumentMessage", rawEvt, nil, file_name, data)
			if err != nil {
				postError("Message.GetDocumentMessage", "Failed to save document", rawEvt)
				return
			}
			return
		}

		sticker := evt.Message.GetStickerMessage()
		if sticker != nil {
			data, err := cli.Download(sticker)
			if err != nil {
				postError("Message.GetStickerMessage", "Failed to download sticker", rawEvt)
				return
			}
			exts, _ := mime.ExtensionsByType(sticker.GetMimetype())
			file_name := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			err = postEventFile("Message.GetStickerMessage", rawEvt, nil, file_name, data)
			if err != nil {
				postError("Message.GetStickerMessage", "Failed to save sticker", rawEvt)
				return
			}
			return
		}

		contact := evt.Message.GetContactMessage()
		if contact != nil {
			file_name := fmt.Sprintf("%s%s", evt.Info.ID, ".vcf")
			err := postEventFile("Message.GetContactMessage", rawEvt, nil, file_name, []byte(*contact.Vcard))
			if err != nil {
				postError("Message.GetContactMessage", "Failed to save contact", rawEvt)
				return
			}
			return
		}

		postEvent("Message", rawEvt, nil)
		return

	case *events.Receipt:
		if evt.Type == events.ReceiptTypeRead || evt.Type == events.ReceiptTypeReadSelf {
			postEvent("ReceiptRead", rawEvt, nil)
		} else if evt.Type == events.ReceiptTypeDelivered {
			postEvent("ReceiptDelivered", rawEvt, nil)
		}
		return
	case *events.Presence:
		if evt.Unavailable {
			if evt.LastSeen.IsZero() {
				postEvent("PresenceOffline", rawEvt, nil)
			} else {
				postEvent("PresenceOfflineLastSeem", rawEvt, nil)
			}
		} else {
			postEvent("PresenceOnline", rawEvt, nil)
		}
		return
	case *events.HistorySync:
		id := atomic.AddInt32(&historySyncID, 1)
		fileName := fmt.Sprintf("%s/history-%d-%d.json", *historyPath, startupTime, id)
		file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			postError("HistorySync", "Failed to open file", rawEvt)
			return
		}
		enc := json.NewEncoder(file)
		enc.SetIndent("", "  ")
		err = enc.Encode(evt.Data)
		if err != nil {
			postError("HistorySync", "Failed to encode JSON", rawEvt)
			return
		}
		postEvent("HistorySync", rawEvt, nil)
		_ = file.Close()
		return
	case *events.AppState:
		postEvent("AppState", rawEvt, nil)
		return
	case *events.KeepAliveTimeout:
		postEvent("KeepAliveTimeout", rawEvt, nil)
		return
	case *events.KeepAliveRestored:
		postEvent("KeepAliveRestored", rawEvt, nil)
		return
	case *events.Blocklist:
		postEvent("Blocklist", rawEvt, nil)
		return
	}
	postEvent("UnknowEvent", rawEvt, nil)
}

type errorPayload struct {
	EvtType  string      `json:"type"`
	EvtError string      `json:"error"`
	Raw      interface{} `json:"raw"`
}

type eventPayload struct {
	Type  string      `json:"type"`
	Raw   interface{} `json:"raw"`
	Extra interface{} `json:"extra,omitempty"`
}

func postEventFile(evt_type string, raw interface{}, extra interface{}, file_name string, file_bytes []byte) error {
	url := fmt.Sprintf("%s/api/collections/events/records", *pocketbaseURL)

	jsonRaw, err := json.Marshal(raw)
	if err != nil {
		return err
	}

	jsonExtra, err := json.Marshal(extra)
	if err != nil {
		return err
	}

	client := &http.Client{}
	// New multipart writer.
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fw, err := writer.CreateFormField("type")
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, strings.NewReader(evt_type))
	if err != nil {
		return err
	}
	fw, err = writer.CreateFormField("raw")
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, strings.NewReader(string(jsonRaw)))
	if err != nil {
		return err
	}
	fw, err = writer.CreateFormField("extra")
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, strings.NewReader(string(jsonExtra)))
	if err != nil {
		return err
	}
	fw, err = writer.CreateFormFile("file", file_name)
	if err != nil {
		return err
	}
	fileReader := bytes.NewReader(file_bytes)
	_, err = io.Copy(fw, fileReader)
	if err != nil {
		return err
	}
	// Close multipart writer.
	writer.Close()
	req, err := http.NewRequest("POST", url, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rsp, _ := client.Do(req)
	if rsp.StatusCode != http.StatusOK {
		log.Errorf("Request failed with response code: %d", rsp.StatusCode)
	}
	return nil
}

func postEvent(evt_type string, raw interface{}, extra interface{}) error {
	url := fmt.Sprintf("%s/api/collections/events/records", *pocketbaseURL)

	payload := eventPayload{
		Type:  evt_type,
		Raw:   raw,
		Extra: extra,
	}
	// Convert payload to JSON
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// Create a new HTTP request with the JSON payload
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return err
	}

	// Set the Content-Type header to application/json
	req.Header.Set("Content-Type", "application/json")

	// Send the HTTP request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check the response status code
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

func postError(evt_type string, evt_error string, raw interface{}) error {
	url := fmt.Sprintf("%s/api/collections/errors/records", *pocketbaseURL)

	payload := errorPayload{
		EvtType:  evt_type,
		EvtError: evt_error,
		Raw:      raw,
	}
	// Convert payload to JSON
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// Create a new HTTP request with the JSON payload
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return err
	}

	// Set the Content-Type header to application/json
	req.Header.Set("Content-Type", "application/json")

	// Send the HTTP request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check the response status code
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}
