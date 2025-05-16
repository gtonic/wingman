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
	accountNumber         string
	completer             provider.Completer
	pollInterval          time.Duration
	receiveMode           string
	whitelistedNumbersMap map[string]struct{}
	isWhitelistActive     bool
	conversationHistories map[string][]provider.Message // In-memory fallback
	maxHistoryMessages    int
	historyStorageType    string
	historySQLitePath     string
	sqlDB                 *sql.DB
	messagePrefixTrigger  string
}

type Config struct {
	URL                  string
	AccountNumber        string
	Completer            provider.Completer
	PollInterval         time.Duration
	ReceiveMode          string
	WhitelistedNumbers   []string
	MaxHistoryMessages   int
	HistoryStorageType   string
	HistorySQLitePath    string
	MessagePrefixTrigger string
}

func New(id string, cfg Config) (*Connector, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("signal connector: url is required")
	}
	if cfg.AccountNumber == "" {
		return nil, fmt.Errorf("signal connector: account_number is required")
	}
	if cfg.Completer == nil {
		return nil, fmt.Errorf("signal connector: completer is required")
	}

	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}

	receiveMode := strings.ToLower(cfg.ReceiveMode)
	if receiveMode == "" {
		receiveMode = "poll"
	}
	if receiveMode != "poll" && receiveMode != "websocket" {
		return nil, fmt.Errorf("signal connector %s: invalid receive_mode '%s'", id, cfg.ReceiveMode)
	}

	whitelistedMap := make(map[string]struct{})
	isWhitelistActive := len(cfg.WhitelistedNumbers) > 0
	if isWhitelistActive {
		for _, num := range cfg.WhitelistedNumbers {
			if num != "" {
				whitelistedMap[num] = struct{}{}
			}
		}
		log.Printf("Signal connector (ID: %s): Whitelist active. Allowed: %v", id, cfg.WhitelistedNumbers)
	} else {
		log.Printf("Signal connector (ID: %s): Whitelist empty. Default 'Note to Self' only.", id)
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

	return &Connector{
		id:                    id,
		client:                http.DefaultClient,
		url:                   strings.TrimSuffix(cfg.URL, "/"),
		accountNumber:         cfg.AccountNumber,
		completer:             cfg.Completer,
		pollInterval:          cfg.PollInterval,
		receiveMode:           receiveMode,
		whitelistedNumbersMap: whitelistedMap,
		isWhitelistActive:     isWhitelistActive,
		conversationHistories: make(map[string][]provider.Message),
		maxHistoryMessages:    maxHistory,
		historyStorageType:    historyStorage,
		historySQLitePath:     actualSQLitePath,
		sqlDB:                 db,
		messagePrefixTrigger:  messagePrefixTrigger,
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
	log.Printf("Starting Signal connector (ID: %s), account: %s, mode: %s, URL: %s, poll_interval: %v, history: %s, prefix: '%s'", c.id, c.accountNumber, c.receiveMode, c.url, c.pollInterval, c.historyStorageType, c.messagePrefixTrigger)

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
		go func() { // Goroutine to close WebSocket on context cancellation
			<-ctx.Done()
			log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled, closing WebSocket.", c.id)
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "context cancelled"))
			conn.Close()
			close(connClosed)
		}()
		for { // Message read loop
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Error reading message: %v.", c.id, err)
				select {
				case <-connClosed: // Already closed by the context cancellation goroutine
				default:
					conn.Close()
					close(connClosed)
				}
				break // Break inner loop to trigger reconnection
			}
			// ... (message processing switch remains the same) ...
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
				case <-connClosed: // Already closed by the context cancellation goroutine
				default:
					conn.Close()
					close(connClosed)
				}
				break // Break inner loop
			default:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received unknown message type: %d", c.id, messageType)
			}

			select { // Check context after processing each message
			case <-ctx.Done():
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled during message processing loop. Stopping.", c.id)
				return ctx.Err() // Exit runWebSocketListener
			default:
			}
		}
	}
}

// processReceivedMessagePayload unmarshals and processes a single message payload.
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

	var sender, messageText, actualMessageForLLM string
	var isNoteToSelf, isDataMessageFromOther bool

	if receivedMsg.Envelope != nil {
		if receivedMsg.Envelope.SyncMessage != nil &&
			receivedMsg.Envelope.SyncMessage.SentMessage != nil &&
			receivedMsg.Envelope.SyncMessage.SentMessage.Message != "" &&
			receivedMsg.Envelope.Source == c.accountNumber &&
			(receivedMsg.Envelope.SyncMessage.SentMessage.Destination == c.accountNumber || receivedMsg.Envelope.SyncMessage.SentMessage.DestinationNumber == c.accountNumber) {
			sender = receivedMsg.Envelope.Source
			messageText = receivedMsg.Envelope.SyncMessage.SentMessage.Message
			isNoteToSelf = true
		} else if receivedMsg.Envelope.DataMessage != nil && receivedMsg.Envelope.DataMessage.Message != "" {
			if receivedMsg.Envelope.Source != "" {
				sender = receivedMsg.Envelope.Source
				messageText = receivedMsg.Envelope.DataMessage.Message
				if sender != c.accountNumber {
					isDataMessageFromOther = true
				} else {
					log.Printf("Signal connector (ID: %s): Ignoring DataMessage from self to other(s) (%s). Text: \"%s\"", c.id, sender, messageText)
				}
			} else {
				log.Printf("Warning: Signal DataMessage (ID: %s) without sender. Data: %+v", c.id, receivedMsg.Envelope.DataMessage)
			}
		}
	}

	shouldProcessBasedOnSender := false
	if isNoteToSelf {
		log.Printf("Signal connector (ID: %s): Identified 'Note to Self' from %s. Text: \"%s\"", c.id, sender, messageText)
		shouldProcessBasedOnSender = true
	} else if isDataMessageFromOther {
		if !c.isWhitelistActive {
			log.Printf("Signal connector (ID: %s): Whitelist inactive. Ignoring message from external sender %s.", c.id, sender)
		} else {
			if _, isWhitelisted := c.whitelistedNumbersMap[sender]; isWhitelisted {
				log.Printf("Signal connector (ID: %s): Sender %s is whitelisted. Text: \"%s\"", c.id, sender, messageText)
				shouldProcessBasedOnSender = true
			} else {
				log.Printf("Signal connector (ID: %s): Sender %s not in whitelist. Ignoring. Text: \"%s\"", c.id, sender, messageText)
			}
		}
	}

	if !shouldProcessBasedOnSender {
		if !isNoteToSelf && !isDataMessageFromOther { // Log only if it wasn't an explicitly ignored type
			log.Printf("Signal connector (ID: %s): Not a processable text message type or sender. Payload: %s", c.id, string(payload))
		}
		return nil
	}

	// Apply prefix trigger logic
	actualMessageForLLM = messageText
	if c.messagePrefixTrigger != "" {
		trimmedMessage := strings.TrimSpace(messageText)
		prefixWithSpace := c.messagePrefixTrigger + " "
		if strings.HasPrefix(trimmedMessage, prefixWithSpace) {
			actualMessageForLLM = strings.TrimSpace(strings.TrimPrefix(trimmedMessage, prefixWithSpace))
			if actualMessageForLLM == "" {
				log.Printf("Signal connector (ID: %s): Message from %s had prefix but no content after. Ignoring.", c.id, sender)
				return nil
			}
			log.Printf("Signal connector (ID: %s): Prefix '%s' matched. Content for LLM: \"%s\"", c.id, c.messagePrefixTrigger, actualMessageForLLM)
		} else if trimmedMessage == c.messagePrefixTrigger {
			log.Printf("Signal connector (ID: %s): Message from %s was only prefix trigger '%s'. Ignoring.", c.id, sender, c.messagePrefixTrigger)
			return nil
		} else {
			log.Printf("Signal connector (ID: %s): Message from %s: \"%s\" does not start with prefix '%s'. Ignoring.", c.id, sender, messageText, c.messagePrefixTrigger)
			return nil
		}
	}

	if sender == "" || actualMessageForLLM == "" { // Should be caught by prefix logic if actualMessageForLLM is empty
		log.Printf("Signal connector (ID: %s): Sender or message for LLM is empty. Sender: '%s'. OriginalText: '%s'. Payload: %s", c.id, sender, messageText, string(payload))
		return nil
	}

	log.Printf("Signal connector (ID: %s): Processing for LLM. Sender: %s, Message: \"%s\"", c.id, sender, actualMessageForLLM)

	var history []provider.Message
	var err error
	currentUserMessage := provider.UserMessage(actualMessageForLLM)

	if c.historyStorageType == "sqlite" {
		dbUserMessage := dbMessage{
			ConversationID:   sender,
			MessageTimestamp: time.Now().UnixMilli(),
			MessageRole:      string(currentUserMessage.Role),
			MessageContent:   currentUserMessage.Text(),
		}
		if errAdd := c.addMessageToSQLite(ctx, dbUserMessage); errAdd != nil {
			log.Printf("Signal connector (ID: %s): Failed to add user message to SQLite for %s: %v. Using current message only.", c.id, sender, errAdd)
			history = []provider.Message{currentUserMessage}
		} else {
			history, err = c.getHistoryFromSQLite(ctx, sender)
			if err != nil {
				log.Printf("Signal connector (ID: %s): Failed to get SQLite history for %s: %v. Using current message only.", c.id, sender, err)
				history = []provider.Message{currentUserMessage}
			}
		}
	} else {
		history, _ = c.conversationHistories[sender]
		history = append(history, currentUserMessage)
		if len(history) > c.maxHistoryMessages {
			startIndex := len(history) - c.maxHistoryMessages
			history = history[startIndex:]
			log.Printf("Signal connector (ID: %s): Memory history for %s truncated to %d messages.", c.id, sender, c.maxHistoryMessages)
		}
		c.conversationHistories[sender] = history
	}

	log.Printf("Signal connector (ID: %s): Sending to LLM for %s (history len %d): %+v", c.id, sender, len(history), history)

	completeCtx, cancelComplete := context.WithTimeout(ctx, 30*time.Second)
	defer cancelComplete()

	llmResponse, err := c.completer.Complete(completeCtx, history, nil)
	if err != nil {
		log.Printf("Error from LLM completer for %s: %v", sender, err)
		return err
	}

	if llmResponse != nil && llmResponse.Message != nil && llmResponse.Message.Text() != "" {
		replyText := llmResponse.Message.Text()
		assistantMessage := *llmResponse.Message
		log.Printf("Signal connector (ID: %s): Received from LLM for %s: \"%s\"", c.id, sender, replyText)

		if c.historyStorageType == "sqlite" {
			dbAssistantMessage := dbMessage{
				ConversationID:   sender,
				MessageTimestamp: time.Now().UnixMilli(),
				MessageRole:      string(assistantMessage.Role),
				MessageContent:   assistantMessage.Text(),
			}
			if errAdd := c.addMessageToSQLite(ctx, dbAssistantMessage); errAdd != nil {
				log.Printf("Signal connector (ID: %s): Failed to add assistant message to SQLite for %s: %v.", c.id, sender, errAdd)
			}
		} else {
			history = append(history, assistantMessage)
			if len(history) > c.maxHistoryMessages {
				startIndex := len(history) - c.maxHistoryMessages
				history = history[startIndex:]
				log.Printf("Signal connector (ID: %s): Memory history for %s (after reply) truncated to %d messages.", c.id, sender, c.maxHistoryMessages)
			}
			c.conversationHistories[sender] = history
		}

		if err := c.sendSignalMessage(ctx, sender, replyText); err != nil {
			log.Printf("Error sending Signal reply to %s: %v", sender, err)
			return err
		}
		log.Printf("Signal connector (ID: %s): Successfully sent reply to %s: \"%s\"", c.id, sender, replyText)
	} else {
		if llmResponse == nil || llmResponse.Message == nil {
			log.Printf("Signal connector (ID: %s): LLM response or message was nil for %s.", c.id, sender)
		} else {
			log.Printf("Signal connector (ID: %s): LLM provided empty reply for %s.", c.id, sender)
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

func (c *Connector) sendSignalMessage(ctx context.Context, recipient string, message string) error {
	sendURL, err := url.JoinPath(c.url, "/v2/send")
	if err != nil {
		return fmt.Errorf("failed to create send URL: %w", err)
	}
	requestBody := SendMessageV2Request{
		Number:     c.accountNumber,
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
