package signal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3" // SQLite driver

	"github.com/adrianliechti/wingman/pkg/connector"
	"github.com/adrianliechti/wingman/pkg/provider"
)

var _ connector.Provider = (*Connector)(nil)

const (
	defaultReconnectInterval = 5 * time.Second
	maxReconnectInterval     = 60 * time.Second
	signalHistoryTableName   = "signal_conversation_history"
	signalHistoryTableSchema = `
CREATE TABLE IF NOT EXISTS %s (
	conversation_id TEXT NOT NULL,
	message_timestamp INTEGER NOT NULL,
	message_role TEXT NOT NULL,
	message_content TEXT NOT NULL,
	PRIMARY KEY (conversation_id, message_timestamp)
);`
)

type Connector struct {
	id                    string
	client                *http.Client
	url                   string
	accountNumber         string // E.164 phone number
	accountUsername       string // Optional: Signal Username (name.01)
	completer             provider.Completer
	pollInterval          time.Duration
	receiveMode           string
	conversationHistories map[string][]provider.Message
	maxHistoryMessages    int
	historyStorageType    string
	historySQLitePath     string
	sqlDB                 *sql.DB
	messagePrefixTrigger  string
	llmTimeoutSeconds     int // Timeout for LLM completion in seconds
}

type Config struct {
	URL                  string
	AccountNumber        string // E.164 phone number
	AccountUsername      string // Optional: Signal Username (name.01)
	Completer            provider.Completer
	PollInterval         time.Duration
	ReceiveMode          string
	MaxHistoryMessages   int
	HistoryStorageType   string
	HistorySQLitePath    string
	MessagePrefixTrigger string
	LLMTimeoutSeconds    int // Timeout for LLM completion in seconds
}

func New(id string, cfg Config) (*Connector, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("signal connector: url is required")
	}
	if cfg.AccountNumber == "" {
		return nil, fmt.Errorf("signal connector: account_number (E.164 phone) is required")
	}
	if cfg.Completer == nil {
		return nil, fmt.Errorf("signal connector: completer is required")
	}

	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}

	llmTimeout := cfg.LLMTimeoutSeconds
	if llmTimeout <= 0 {
		llmTimeout = 160 // Default to 160 seconds if not set or invalid
	}

	receiveMode := strings.ToLower(cfg.ReceiveMode)
	if receiveMode == "" {
		receiveMode = "poll"
	}
	if receiveMode != "poll" && receiveMode != "websocket" {
		return nil, fmt.Errorf("signal connector %s: invalid receive_mode '%s'", id, cfg.ReceiveMode)
	}

	maxHistory := cfg.MaxHistoryMessages
	if maxHistory <= 0 {
		maxHistory = 20
		log.Printf("Signal connector (ID: %s): MaxHistoryMessages invalid, defaulting to %d", id, maxHistory)
	}

	historyStorage := strings.ToLower(cfg.HistoryStorageType)
	if historyStorage == "" {
		historyStorage = "memory"
	}
	if historyStorage != "memory" && historyStorage != "sqlite" {
		return nil, fmt.Errorf("signal connector %s: invalid history_storage_type '%s'", id, cfg.HistoryStorageType)
	}
	log.Printf("Signal connector (ID: %s): Using history storage: %s", id, historyStorage)

	var db *sql.DB
	var err error
	actualSQLitePath := cfg.HistorySQLitePath
	if historyStorage == "sqlite" {
		if actualSQLitePath == "" {
			actualSQLitePath = "signal_history_" + strings.ReplaceAll(id, "/", "_") + ".db"
			log.Printf("Signal connector (ID: %s): HistorySQLitePath not set, defaulting to %s", id, actualSQLitePath)
		}
		db, err = sql.Open("sqlite3", actualSQLitePath)
		if err != nil {
			log.Printf("Signal connector (ID: %s): Failed to open SQLite DB at %s: %v. Falling back to memory.", id, actualSQLitePath, err)
			historyStorage = "memory"
			db = nil
		} else {
			log.Printf("Signal connector (ID: %s): Opened SQLite DB at %s", id, actualSQLitePath)
			if errPing := db.Ping(); errPing != nil {
				log.Printf("Signal connector (ID: %s): Failed to ping SQLite DB at %s: %v. Falling back to memory.", id, actualSQLitePath, errPing)
				db.Close()
				db = nil
				historyStorage = "memory"
			}
		}
	}

	messagePrefixTrigger := strings.TrimSpace(cfg.MessagePrefixTrigger)
	if messagePrefixTrigger != "" {
		log.Printf("Signal connector (ID: %s): Message prefix trigger enabled: '%s'", id, messagePrefixTrigger)
	}

	accountUsername := strings.TrimSpace(cfg.AccountUsername)
	if accountUsername != "" {
		log.Printf("Signal connector (ID: %s): Account Username configured: '%s' (will be used as sender for group messages if set)", id, accountUsername)
	}

	return &Connector{
		id:                    id,
		client:                http.DefaultClient,
		url:                   strings.TrimSuffix(cfg.URL, "/"),
		accountNumber:         cfg.AccountNumber,
		accountUsername:       accountUsername,
		completer:             cfg.Completer,
		pollInterval:          cfg.PollInterval,
		receiveMode:           receiveMode,
		conversationHistories: make(map[string][]provider.Message),
		maxHistoryMessages:    maxHistory,
		historyStorageType:    historyStorage,
		historySQLitePath:     actualSQLitePath,
		sqlDB:                 db,
		messagePrefixTrigger:  messagePrefixTrigger,
		llmTimeoutSeconds:     llmTimeout,
	}, nil
}

func (c *Connector) ID() string {
	return c.id
}

func (c *Connector) initializeSQLiteDB(ctx context.Context) error {
	if c.sqlDB == nil {
		return fmt.Errorf("SQLite DB connection not initialized for connector %s", c.id)
	}
	query := fmt.Sprintf(signalHistoryTableSchema, signalHistoryTableName)
	log.Printf("Signal connector (ID: %s): Ensuring SQLite table '%s' exists.", c.id, signalHistoryTableName)
	_, err := c.sqlDB.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create/ensure table '%s': %w", signalHistoryTableName, err)
	}
	log.Printf("Signal connector (ID: %s): SQLite table '%s' ensured.", c.id, signalHistoryTableName)
	return nil
}

func (c *Connector) Close() error {
	if c.sqlDB != nil {
		log.Printf("Signal connector (ID: %s): Closing SQLite DB connection to %s.", c.id, c.historySQLitePath)
		return c.sqlDB.Close()
	}
	return nil
}

func (c *Connector) Start(ctx context.Context) error {
	log.Printf("Starting Signal connector (ID: %s), account: %s (username: '%s'), mode: %s, URL: %s, poll_interval: %v, history: %s, prefix: '%s'",
		c.id, c.accountNumber, c.accountUsername, c.receiveMode, c.url, c.pollInterval, c.historyStorageType, c.messagePrefixTrigger)

	if c.historyStorageType == "sqlite" {
		if c.sqlDB == nil {
			log.Printf("Signal connector (ID: %s): SQLite mode configured but DB connection is nil. Falling back to memory.", c.id)
			c.historyStorageType = "memory"
		} else {
			if err := c.initializeSQLiteDB(ctx); err != nil {
				log.Printf("Signal connector (ID: %s): Failed to initialize SQLite DB table: %v. Falling back to memory.", c.id, err)
				c.sqlDB.Close()
				c.sqlDB = nil
				c.historyStorageType = "memory"
			} else {
				log.Printf("Signal connector (ID: %s): SQLite history mode initialized.", c.id)
				defer c.Close()
			}
		}
	}

	if c.receiveMode == "websocket" {
		return c.runWebSocketListener(ctx)
	}

	log.Printf("Signal connector (ID: %s): Using POLL mode.", c.id)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	log.Printf("Signal connector (ID: %s): Ticker created for POLL mode. Interval: %v", c.id, c.pollInterval)

	for {
		select {
		case <-ctx.Done():
			log.Printf("Stopping Signal connector (ID: %s) (POLL mode): context cancelled: %v", c.id, ctx.Err())
			return ctx.Err()
		case tickTime := <-ticker.C:
			log.Printf("Signal connector (ID: %s) (POLL mode): Tick received at %v, attempting to poll...", c.id, tickTime)
			if err := c.pollAndProcessMessages(ctx); err != nil {
				log.Printf("Error polling Signal messages (ID: %s) (POLL mode): %v", c.id, err)
			}
		}
	}
}

func (c *Connector) pollAndProcessMessages(ctx context.Context) error {
	receiveURL, err := url.JoinPath(c.url, "/v1/receive/", c.accountNumber)
	if err != nil {
		return fmt.Errorf("failed to create receive URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", receiveURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request to %s: %w", receiveURL, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to GET %s: %w", receiveURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to receive messages, status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	var messageStrings []string
	if err := json.NewDecoder(resp.Body).Decode(&messageStrings); err != nil {
		bodyBytes, _ := io.ReadAll(resp.Body)
		if len(bodyBytes) > 0 {
			return fmt.Errorf("failed to decode received message strings: %w. Body: %s", err, string(bodyBytes))
		}
		return nil
	}
	if len(messageStrings) == 0 {
		return nil
	}
	log.Printf("Signal connector (ID: %s) (POLL mode) received %d raw message strings/events.", c.id, len(messageStrings))
	for _, msgStr := range messageStrings {
		if err := c.processReceivedMessagePayload(ctx, []byte(msgStr)); err != nil {
			log.Printf("Signal connector (ID: %s) (POLL mode) error processing message payload: %v. Raw string: %s", c.id, err, msgStr)
		}
	}
	return nil
}

func (c *Connector) runWebSocketListener(ctx context.Context) error {
	wsScheme := "ws"
	if strings.HasPrefix(c.url, "https://") {
		wsScheme = "wss"
	}
	httpBaseURL, err := url.Parse(c.url)
	if err != nil {
		log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Invalid base URL %s: %v", c.id, c.url, err)
		return err
	}
	wsURL := url.URL{
		Scheme: wsScheme,
		Host:   httpBaseURL.Host,
		Path:   "/v1/receive/" + c.accountNumber,
	}
	urlString := wsURL.String()
	log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Attempting to connect to %s", c.id, urlString)
	var currentReconnectDelay = defaultReconnectInterval
	for {
		select {
		case <-ctx.Done():
			log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled before connection. Stopping.", c.id)
			return ctx.Err()
		default:
		}
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, urlString, nil)
		if err != nil {
			log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Failed to connect to %s: %v. Retrying in %v...", c.id, urlString, err, currentReconnectDelay)
			select {
			case <-time.After(currentReconnectDelay):
				currentReconnectDelay *= 2
				if currentReconnectDelay > maxReconnectInterval {
					currentReconnectDelay = maxReconnectInterval
				}
				continue
			case <-ctx.Done():
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled during reconnect wait. Stopping.", c.id)
				return ctx.Err()
			}
		}
		log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Successfully connected to %s", c.id, urlString)
		currentReconnectDelay = defaultReconnectInterval
		connClosed := make(chan struct{})
		go func() {
			<-ctx.Done()
			log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled, closing WebSocket.", c.id)
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "context cancelled"))
			conn.Close()
			close(connClosed)
		}()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Error reading message: %v.", c.id, err)
				select {
				case <-connClosed:
				default:
					conn.Close()
					close(connClosed)
				}
				break
			}
			switch messageType {
			case websocket.TextMessage:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received text message: %s", c.id, string(payload))
				if err := c.processReceivedMessagePayload(ctx, payload); err != nil {
					log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) error processing message payload: %v. Payload: %s", c.id, err, string(payload))
				}
			case websocket.BinaryMessage:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received binary message (length: %d). Assuming JSON string, attempting to process.", c.id, len(payload))
				if err := c.processReceivedMessagePayload(ctx, payload); err != nil {
					log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) error processing binary message payload: %v.", c.id, err)
				}
			case websocket.PingMessage:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received ping.", c.id)
			case websocket.PongMessage:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received pong.", c.id)
			case websocket.CloseMessage:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received close message from server.", c.id)
				select {
				case <-connClosed:
				default:
					conn.Close()
					close(connClosed)
				}
				break
			default:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received unknown message type: %d", c.id, messageType)
			}

			select {
			case <-ctx.Done():
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled during message processing loop. Stopping.", c.id)
				return ctx.Err()
			default:
			}
		}
	}
}

func (c *Connector) downloadAttachment(ctx context.Context, attachmentID string) ([]byte, error) {
	// Attempt to download the attachment from the Signal REST API
	attachmentURL, err := url.JoinPath(c.url, "/v1/attachments/", attachmentID)
	if err != nil {
		return nil, fmt.Errorf("failed to create attachment URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", attachmentURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GET request for attachment: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to GET attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to download attachment, status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment body: %w", err)
	}
	return data, nil
}

func (c *Connector) processReceivedMessagePayload(ctx context.Context, payload []byte) error {
	log.Printf("Signal connector (ID: %s) processing raw payload: %s", c.id, string(payload))

	var receivedMsg ReceivedSignalMessage
	if err := json.Unmarshal(payload, &receivedMsg); err != nil {
		log.Printf("Error unmarshalling Signal message payload (ID: %s): %v. Raw payload: %s", c.id, err, string(payload))
		if len(payload) > 0 {
			return fmt.Errorf("failed to unmarshal non-empty payload: %w", err)
		}
		log.Printf("Signal connector (ID: %s) received empty payload, skipping.", c.id)
		return nil
	}

	log.Printf("Signal connector (ID: %s) successfully unmarshalled. Account: %s, Envelope: %+v, CallMessage: %+v", c.id, receivedMsg.Account, receivedMsg.Envelope, receivedMsg.CallMessage)

	var actualSender, messageText, conversationContextID, actualMessageForLLM string
	isFromSelf := false
	var messageSourceType string // "dataMessage", "sentMessageSync1on1", "sentMessageSyncGroup"
	var attachments []Attachment // Using the Attachment struct from api.go

	if receivedMsg.Envelope == nil || receivedMsg.Envelope.Source == "" {
		log.Printf("Signal connector (ID: %s): Envelope or source is missing. Cannot process. Payload: %s", c.id, string(payload))
		return nil
	}
	// Override sourceName with accountUsername if available
	if c.accountUsername != "" {
		receivedMsg.Envelope.SourceName = c.accountUsername
	}
	actualSender = receivedMsg.Envelope.Source
	isFromSelf = (actualSender == c.accountNumber)

	// Extract message text and attachments
	if receivedMsg.Envelope.DataMessage != nil {
		messageSourceType = "dataMessage"
		messageText = receivedMsg.Envelope.DataMessage.Message
		attachments = receivedMsg.Envelope.DataMessage.Attachments // Get attachments

		if receivedMsg.Envelope.DataMessage.GroupInfo != nil && receivedMsg.Envelope.DataMessage.GroupInfo.GroupID != "" {
			conversationContextID = receivedMsg.Envelope.DataMessage.GroupInfo.GroupID
			log.Printf("Signal connector (ID: %s): DataMessage from group %s by sender %s.", c.id, conversationContextID, actualSender)
		} else {
			conversationContextID = actualSender
			log.Printf("Signal connector (ID: %s): DataMessage from direct sender %s.", c.id, actualSender)
		}
	} else if isFromSelf && receivedMsg.Envelope.SyncMessage != nil && receivedMsg.Envelope.SyncMessage.SentMessage != nil {
		messageText = receivedMsg.Envelope.SyncMessage.SentMessage.Message
		attachments = receivedMsg.Envelope.SyncMessage.SentMessage.Attachments // Get attachments

		if receivedMsg.Envelope.SyncMessage.SentMessage.GroupInfo != nil && receivedMsg.Envelope.SyncMessage.SentMessage.GroupInfo.GroupID != "" {
			messageSourceType = "sentMessageSyncGroup"
			conversationContextID = receivedMsg.Envelope.SyncMessage.SentMessage.GroupInfo.GroupID
			log.Printf("Signal connector (ID: %s): SentMessage sync by self to group %s.", c.id, conversationContextID)
		} else if receivedMsg.Envelope.SyncMessage.SentMessage.Destination == c.accountNumber || receivedMsg.Envelope.SyncMessage.SentMessage.DestinationNumber == c.accountNumber {
			messageSourceType = "sentMessageSync1on1"
			conversationContextID = c.accountNumber
			log.Printf("Signal connector (ID: %s): SentMessage sync by self to self (Note to Self 1-on-1).", c.id)
		} else {
			log.Printf("Signal connector (ID: %s): SentMessage sync by self to other user %s. Ignoring. Text: \"%s\"", c.id, receivedMsg.Envelope.SyncMessage.SentMessage.Destination, messageText)
			return nil
		}
	} else {
		log.Printf("Signal connector (ID: %s): Not a processable DataMessage or relevant SentMessage sync (no text or attachments). Payload: %s", c.id, string(payload))
		return nil
	}

	// Process attachments
	hasImageAttachment := false
	var imageFileContent *provider.File
	if len(attachments) > 0 {
		log.Printf("Signal connector (ID: %s): Found %d attachments for message from %s (context %s).", c.id, len(attachments), actualSender, conversationContextID)
		for _, att := range attachments {
			log.Printf("Signal connector (ID: %s): Attachment details: Filename: %s, ContentType: %s, Size: %d", c.id, att.Filename, att.ContentType, att.Size)
			if strings.HasPrefix(strings.ToLower(att.ContentType), "image/") {
				hasImageAttachment = true
				// Download the image
				imageData, err := c.downloadAttachment(ctx, att.ID)
				if err != nil {
					log.Printf("Signal connector (ID: %s): Failed to download image attachment %s: %v", c.id, att.ID, err)
					imageNotification := fmt.Sprintf("[Image Received but failed to download: %s]", att.ContentType)
					if messageText == "" {
						messageText = imageNotification
					} else {
						messageText = messageText + " " + imageNotification
					}
				} else {
					imageFileContent = &provider.File{
						Name:        att.Filename,
						Content:     imageData,
						ContentType: att.ContentType,
					}
					log.Printf("Signal connector (ID: %s): Image attachment downloaded and will be sent to LLM (%s, %d bytes).", c.id, att.Filename, len(imageData))
				}
				break // Only process the first image
			}
		}
	}

	// If there's no text message and no image attachment processed, it might be an unsupported attachment type or empty message.
	if messageText == "" && !hasImageAttachment {
		log.Printf("Signal connector (ID: %s): Message from %s (context %s) has no text content or processable image attachment. Ignoring. Payload: %s", c.id, actualSender, conversationContextID, string(payload))
		return nil
	}

	shouldProcessForLLM := false
	if messageSourceType == "sentMessageSync1on1" {
		shouldProcessForLLM = true
		log.Printf("Signal connector (ID: %s): Processing 'Note to Self' 1-on-1.", c.id)
	} else if messageSourceType == "sentMessageSyncGroup" {
		log.Printf("Signal connector (ID: %s): Processing message from self to group %s.", c.id, conversationContextID)
		shouldProcessForLLM = true
	} else if messageSourceType == "dataMessage" {
		if isFromSelf {
			if conversationContextID != actualSender && conversationContextID != "" { // Group message from self
				log.Printf("Signal connector (ID: %s): Processing DataMessage from self to group %s.", c.id, conversationContextID)
				shouldProcessForLLM = true
			} else {
				log.Printf("Signal connector (ID: %s): Processing DataMessage from self (not 'Note to Self' or group). Sender: %s, Context: %s", c.id, actualSender, conversationContextID)
				shouldProcessForLLM = true
			}
		} else { // DataMessage from other
			if conversationContextID != actualSender && conversationContextID != "" { // Group message from other
				log.Printf("Signal connector (ID: %s): Processing DataMessage from other to group %s.", c.id, conversationContextID)
				shouldProcessForLLM = true
			} else {
				log.Printf("Signal connector (ID: %s): Processing DataMessage from external sender %s.", c.id, actualSender)
				shouldProcessForLLM = true
			}
		}
	}

	if !shouldProcessForLLM {
		// Send read receipt after processing the message (even if not processed for LLM)
		if receivedMsg.Envelope != nil && actualSender != "" && receivedMsg.Envelope.Timestamp != 0 {
			if err := c.sendReadReceipt(ctx, actualSender, receivedMsg.Envelope.Timestamp); err != nil {
				log.Printf("Signal connector (ID: %s): Failed to send read receipt to API for %s at %d: %v", c.id, actualSender, receivedMsg.Envelope.Timestamp, err)
			} else {
				log.Printf("Signal connector (ID: %s): Sent read receipt to API for %s at %d", c.id, actualSender, receivedMsg.Envelope.Timestamp)
			}
		}
		return nil
	}

	actualMessageForLLM = messageText
	// Only enforce prefix for non-self messages
	if c.messagePrefixTrigger != "" && messageSourceType != "sentMessageSync1on1" {
		trimmedMessage := strings.TrimSpace(messageText)
		prefixWithSpace := c.messagePrefixTrigger + " "
		if strings.HasPrefix(trimmedMessage, prefixWithSpace) {
			actualMessageForLLM = strings.TrimSpace(strings.TrimPrefix(trimmedMessage, prefixWithSpace))
			if actualMessageForLLM == "" {
				log.Printf("Signal connector (ID: %s): Message from %s (context %s) had prefix but no content after. Ignoring.", c.id, actualSender, conversationContextID)
				return nil
			}
			log.Printf("Signal connector (ID: %s): Prefix '%s' matched for %s (context %s). Content for LLM: \"%s\"", c.id, c.messagePrefixTrigger, actualSender, conversationContextID, actualMessageForLLM)
		} else if trimmedMessage == c.messagePrefixTrigger {
			log.Printf("Signal connector (ID: %s): Message from %s (context %s) was only prefix trigger '%s'. Ignoring.", c.id, actualSender, conversationContextID, c.messagePrefixTrigger)
			return nil
		} else {
			log.Printf("Signal connector (ID: %s): Message from %s (context %s): \"%s\" does not start with prefix '%s'. Ignoring.", c.id, actualSender, conversationContextID, messageText, c.messagePrefixTrigger)
			return nil
		}
	}

	if conversationContextID == "" || actualMessageForLLM == "" {
		log.Printf("Signal connector (ID: %s): Conversation context ID or message for LLM is empty. ContextID: '%s', LLM Msg: '%s'. OriginalText: '%s'. Payload: %s", c.id, conversationContextID, actualMessageForLLM, messageText, string(payload))
		return nil
	}

	log.Printf("Signal connector (ID: %s): Processing for LLM. ContextID: %s, Actual Sender: %s, Message: \"%s\"", c.id, conversationContextID, actualSender, actualMessageForLLM)

	var history []provider.Message
	var err error

	// Build user message with text and (if present) image file content
	var userContents []provider.Content
	if actualMessageForLLM != "" {
		userContents = append(userContents, provider.TextContent(actualMessageForLLM))
	}
	if imageFileContent != nil {
		userContents = append(userContents, provider.FileContent(imageFileContent))
	}
	currentUserMessage := provider.Message{
		Role:    provider.MessageRoleUser,
		Content: userContents,
	}

	if c.historyStorageType == "sqlite" {
		dbUserMessage := dbMessage{
			ConversationID:   conversationContextID,
			MessageTimestamp: time.Now().UnixMilli(),
			MessageRole:      string(currentUserMessage.Role),
			MessageContent:   currentUserMessage.Text(),
		}
		if errAdd := c.addMessageToSQLite(ctx, dbUserMessage); errAdd != nil {
			log.Printf("Signal connector (ID: %s): Failed to add user message to SQLite for %s: %v. Using current message only.", c.id, conversationContextID, errAdd)
			history = []provider.Message{currentUserMessage}
		} else {
			history, err = c.getHistoryFromSQLite(ctx, conversationContextID)
			if err != nil {
				log.Printf("Signal connector (ID: %s): Failed to get SQLite history for %s: %v. Using current message only.", c.id, conversationContextID, err)
				history = []provider.Message{currentUserMessage}
			}
		}
	} else {
		history, _ = c.conversationHistories[conversationContextID]
		history = append(history, currentUserMessage)
		if len(history) > c.maxHistoryMessages {
			startIndex := len(history) - c.maxHistoryMessages
			history = history[startIndex:]
			log.Printf("Signal connector (ID: %s): Memory history for %s truncated to %d messages.", c.id, conversationContextID, c.maxHistoryMessages)
		}
		c.conversationHistories[conversationContextID] = history
	}

	log.Printf("Signal connector (ID: %s): Sending to LLM for context %s (actual sender %s, history len %d): %+v", c.id, conversationContextID, actualSender, len(history), history)

	completeCtx, cancelComplete := context.WithTimeout(ctx, time.Duration(c.llmTimeoutSeconds)*time.Second)
	defer cancelComplete()

	llmResponse, err := c.completer.Complete(completeCtx, history, nil)
	if err != nil {
		log.Printf("Error from LLM completer for context %s (sender %s): %v", c.id, conversationContextID, actualSender, err)
		return err
	}

	if llmResponse != nil && llmResponse.Message != nil && llmResponse.Message.Text() != "" {
		replyText := llmResponse.Message.Text()
		assistantMessage := *llmResponse.Message
		log.Printf("Signal connector (ID: %s): Received from LLM for context %s: \"%s\"", c.id, conversationContextID, replyText)

		if c.historyStorageType == "sqlite" {
			dbAssistantMessage := dbMessage{
				ConversationID:   conversationContextID,
				MessageTimestamp: time.Now().UnixMilli(),
				MessageRole:      string(assistantMessage.Role),
				MessageContent:   assistantMessage.Text(),
			}
			if errAdd := c.addMessageToSQLite(ctx, dbAssistantMessage); errAdd != nil {
				log.Printf("Signal connector (ID: %s): Failed to add assistant message to SQLite for %s: %v.", c.id, conversationContextID, errAdd)
			}
		} else {
			history = append(history, assistantMessage)
			if len(history) > c.maxHistoryMessages {
				startIndex := len(history) - c.maxHistoryMessages
				history = history[startIndex:]
				log.Printf("Signal connector (ID: %s): Memory history for %s (after reply) truncated to %d messages.", c.id, conversationContextID, c.maxHistoryMessages)
			}
			c.conversationHistories[conversationContextID] = history
		}

		// Determine sender for the API call
		apiSender := c.accountNumber
		// For group replies, use accountUsername if set and contains a dot, else fallback to accountNumber
		if conversationContextID != actualSender && conversationContextID != c.accountNumber && c.accountUsername != "" && strings.Contains(c.accountUsername, ".") {
			apiSender = c.accountUsername
			log.Printf("Signal connector (ID: %s): Using account username '%s' as sender for group reply to %s", c.id, apiSender, conversationContextID)
		}

		if err := c.sendSignalMessage(ctx, apiSender, conversationContextID, replyText); err != nil {
			log.Printf("Error sending Signal reply to %s (using sender %s): %v", conversationContextID, apiSender, err)
			return err
		}
		log.Printf("Signal connector (ID: %s): Successfully sent reply to %s (using sender %s): \"%s\"", c.id, conversationContextID, apiSender, replyText)
	} else {
		if llmResponse == nil || llmResponse.Message == nil {
			log.Printf("Signal connector (ID: %s): LLM response or message was nil for context %s.", c.id, conversationContextID)
		} else {
			log.Printf("Signal connector (ID: %s): LLM provided empty reply for context %s.", c.id, conversationContextID)
		}
	}
	// Send read receipt after processing the message
	if receivedMsg.Envelope != nil && actualSender != "" && receivedMsg.Envelope.Timestamp != 0 {
		if err := c.sendReadReceipt(ctx, actualSender, receivedMsg.Envelope.Timestamp); err != nil {
			log.Printf("Signal connector (ID: %s): Failed to send read receipt to API for %s at %d: %v", c.id, actualSender, receivedMsg.Envelope.Timestamp, err)
		} else {
			log.Printf("Signal connector (ID: %s): Sent read receipt to API for %s at %d", c.id, actualSender, receivedMsg.Envelope.Timestamp)
		}
	}
	return nil
}

type dbMessage struct {
	ConversationID   string `json:"conversation_id"`
	MessageTimestamp int64  `json:"message_timestamp"`
	MessageRole      string `json:"message_role"`
	MessageContent   string `json:"message_content"`
}

func (c *Connector) getHistoryFromSQLite(ctx context.Context, conversationID string) ([]provider.Message, error) {
	if c.sqlDB == nil {
		return nil, fmt.Errorf("SQLite DB not initialized for getHistory")
	}
	query := fmt.Sprintf(`
		SELECT message_role, message_content 
		FROM (
			SELECT message_role, message_content, message_timestamp 
			FROM %s 
			WHERE conversation_id = ? 
			ORDER BY message_timestamp DESC 
			LIMIT ?
		) ORDER BY message_timestamp ASC;`, signalHistoryTableName)

	log.Printf("Signal connector (ID: %s): Getting history from SQLite for %s. Query: %s, Limit: %d", c.id, conversationID, query, c.maxHistoryMessages)

	rows, err := c.sqlDB.QueryContext(ctx, query, conversationID, c.maxHistoryMessages)
	if err != nil {
		return nil, fmt.Errorf("SQLite query failed: %w", err)
	}
	defer rows.Close()

	var history []provider.Message
	for rows.Next() {
		var roleStr, contentStr string
		if err := rows.Scan(&roleStr, &contentStr); err != nil {
			return nil, fmt.Errorf("SQLite row scan failed: %w", err)
		}
		var role provider.MessageRole
		switch strings.ToLower(roleStr) {
		case string(provider.MessageRoleUser):
			role = provider.MessageRoleUser
		case string(provider.MessageRoleAssistant):
			role = provider.MessageRoleAssistant
		case string(provider.MessageRoleSystem):
			role = provider.MessageRoleSystem
		default:
			log.Printf("Signal connector (ID: %s): Unknown message role '%s' from DB, defaulting to user.", c.id, roleStr)
			role = provider.MessageRoleUser
		}
		history = append(history, provider.Message{Role: role, Content: []provider.Content{provider.TextContent(contentStr)}})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("SQLite rows iteration error: %w", err)
	}
	log.Printf("Signal connector (ID: %s): Retrieved %d messages from SQLite history for %s.", c.id, len(history), conversationID)
	return history, nil
}

func (c *Connector) addMessageToSQLite(ctx context.Context, msg dbMessage) error {
	if c.sqlDB == nil {
		return fmt.Errorf("SQLite DB not initialized for addMessage")
	}
	query := fmt.Sprintf(`INSERT INTO %s (conversation_id, message_timestamp, message_role, message_content) VALUES (?, ?, ?, ?);`, signalHistoryTableName)
	log.Printf("Signal connector (ID: %s): Adding message to SQLite for %s: Role=%s", c.id, msg.ConversationID, msg.MessageRole)

	_, err := c.sqlDB.ExecContext(ctx, query, msg.ConversationID, msg.MessageTimestamp, msg.MessageRole, msg.MessageContent)
	if err != nil {
		return fmt.Errorf("SQLite insert failed: %w", err)
	}
	return nil
}

func (c *Connector) sendSignalMessage(ctx context.Context, apiSender, recipient string, message string) error {
	sendURL, err := url.JoinPath(c.url, "/v2/send")
	if err != nil {
		return fmt.Errorf("failed to create send URL: %w", err)
	}
	requestBody := SendMessageV2Request{
		Number:     apiSender, // Use the determined apiSender (phone number or username)
		Recipients: []string{recipient},
		Message:    message,
		TextMode:   "normal",
	}
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal send request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", sendURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create POST request to %s: %w", sendURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to POST to %s: %w", sendURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		var sendErr SendMessageError
		if json.Unmarshal(bodyBytes, &sendErr) == nil && sendErr.Error != "" {
			return fmt.Errorf("failed to send message, status %d: %s", resp.StatusCode, sendErr.Error)
		}
		return fmt.Errorf("failed to send message, status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// sendReadReceipt sends a "read" receipt to the API for the given recipient and timestamp.
func (c *Connector) sendReadReceipt(ctx context.Context, recipient string, timestamp int64) error {
	apiURL, err := url.JoinPath(c.url, "/v1/receipts/")
	if err != nil {
		return fmt.Errorf("failed to create receipts URL: %w", err)
	}
	apiURL += url.PathEscape(recipient)
	body := map[string]interface{}{
		"receipt_type": "read",
		"recipient":    recipient,
		"timestamp":    timestamp,
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal read receipt: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create POST request for read receipt: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to POST read receipt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to send read receipt, status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}
