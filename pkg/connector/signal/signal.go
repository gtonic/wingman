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
}

type Config struct {
	URL                string
	AccountNumber      string
	Completer          provider.Completer
	PollInterval       time.Duration
	ReceiveMode        string
	WhitelistedNumbers []string
	MaxHistoryMessages int
	HistoryStorageType string
	HistorySQLitePath  string
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
			db = nil // Ensure db is nil if open failed
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
	log.Printf("Starting Signal connector (ID: %s), account: %s, mode: %s, URL: %s, poll_interval: %v, history: %s", c.id, c.accountNumber, c.receiveMode, c.url, c.pollInterval, c.historyStorageType)

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
	// ... (rest of polling logic remains the same as before) ...
	log.Printf("Signal connector (ID: %s): About to create ticker for POLL mode. Interval value: %v (type: %T)", c.id, c.pollInterval, c.pollInterval)
	ticker := time.NewTicker(c.pollInterval)
	log.Printf("Signal connector (ID: %s): Ticker creation attempted for POLL mode. Ticker object: %p", c.id, ticker)

	defer ticker.Stop()
	log.Printf("Signal connector (ID: %s): Defer ticker.Stop() registered for POLL mode.", c.id)

	log.Printf("Signal connector (ID: %s): Successfully created ticker for POLL mode. Entering main select loop...", c.id)

	for {
		select {
		case <-ctx.Done():
			log.Printf("Stopping Signal connector (ID: %s) (POLL mode): context cancelled: %v", c.id, ctx.Err())
			return ctx.Err()
		case tickTime := <-ticker.C:
			log.Printf("Signal connector (ID: %s) (POLL mode): Tick received at %v, attempting to poll for messages...", c.id, tickTime)
			if err := c.pollAndProcessMessages(ctx); err != nil {
				log.Printf("Error polling Signal messages (ID: %s) (POLL mode): %v", c.id, err)
			}
		}
	}
}

func (c *Connector) pollAndProcessMessages(ctx context.Context) error {
	// ... (this function remains largely the same, calls processReceivedMessagePayload)
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
	// ... (this function remains largely the same, calls processReceivedMessagePayload) ...
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
			log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled before connection attempt. Listener stopping.", c.id)
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
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled during reconnect wait. Listener stopping.", c.id)
				return ctx.Err()
			}
		}
		log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Successfully connected to %s", c.id, urlString)
		currentReconnectDelay = defaultReconnectInterval
		connClosed := make(chan struct{})
		defer func() {
			select {
			case <-connClosed:
			default:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Closing WebSocket connection due to function exit.", c.id)
				conn.Close()
			}
		}()
		go func() {
			select {
			case <-ctx.Done():
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled, closing WebSocket connection.", c.id)
				conn.Close()
				close(connClosed)
			case <-connClosed:
				return
			}
		}()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Error reading message: %v. Will attempt to reconnect.", c.id, err)
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
					// Log more serious errors
				}
				conn.Close()
				close(connClosed)
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
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received close message. Will attempt to reconnect.", c.id)
				conn.Close()
				close(connClosed)
				break
			default:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received unknown message type: %d", c.id, messageType)
			}
			select {
			case <-ctx.Done():
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled during message processing. Listener stopping.", c.id)
				return ctx.Err()
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

	log.Printf("Signal connector (ID: %s) successfully unmarshalled payload. Account: %s, Envelope: %+v, CallMessage: %+v", c.id, receivedMsg.Account, receivedMsg.Envelope, receivedMsg.CallMessage)

	var sender, messageText string
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
			log.Printf("Signal connector (ID: %s): Identified 'Note to Self' (SentMessage sync) from %s. Text: \"%s\"", c.id, sender, messageText)
		} else if receivedMsg.Envelope.DataMessage != nil && receivedMsg.Envelope.DataMessage.Message != "" {
			if receivedMsg.Envelope.Source != "" {
				sender = receivedMsg.Envelope.Source
				messageText = receivedMsg.Envelope.DataMessage.Message
				if sender != c.accountNumber {
					isDataMessageFromOther = true
				} else {
					log.Printf("Signal connector (ID: %s): Ignoring incoming DataMessage originating from self to other(s) (%s). Text: \"%s\"", c.id, sender, messageText)
				}
			} else {
				log.Printf("Warning: Signal DataMessage (ID: %s) received without clear sender in envelope. DataMessage: %+v", c.id, receivedMsg.Envelope.DataMessage)
			}
		}
	}

	shouldProcessForLLM := false
	if isNoteToSelf {
		shouldProcessForLLM = true
	} else if isDataMessageFromOther {
		if !c.isWhitelistActive {
			log.Printf("Signal connector (ID: %s): Whitelist is not active (default 'Note to Self' only mode). Ignoring message from external sender %s.", c.id, sender)
		} else {
			if _, isWhitelisted := c.whitelistedNumbersMap[sender]; isWhitelisted {
				log.Printf("Signal connector (ID: %s): Sender %s is whitelisted. Processing message. Text: \"%s\"", c.id, sender, messageText)
				shouldProcessForLLM = true
			} else {
				log.Printf("Signal connector (ID: %s): Sender %s is not in whitelist. Ignoring message. Text: \"%s\"", c.id, sender, messageText)
			}
		}
	}

	if !shouldProcessForLLM {
		if sender == "" && messageText == "" && (receivedMsg.Envelope == nil || (receivedMsg.Envelope.DataMessage == nil && receivedMsg.Envelope.SyncMessage == nil && receivedMsg.Envelope.ReceiptMessage == nil)) && receivedMsg.CallMessage == nil {
			log.Printf("Signal connector (ID: %s): Received unhandled or non-text event type. Payload: %s", c.id, string(payload))
		}
		return nil
	}

	if sender == "" || messageText == "" {
		log.Printf("Signal connector (ID: %s): Internal logic error - sender or messageText empty despite shouldProcessForLLM=true. Payload: %s", c.id, string(payload))
		return nil
	}

	log.Printf("Signal connector (ID: %s): Preparing to send to LLM. Sender: %s, Message: \"%s\"", c.id, sender, messageText)

	var history []provider.Message
	var err error
	currentUserMessage := provider.UserMessage(messageText)

	if c.historyStorageType == "sqlite" {
		dbUserMessage := dbMessage{
			ConversationID:   sender,
			MessageTimestamp: time.Now().UnixMilli(),
			MessageRole:      string(currentUserMessage.Role),
			MessageContent:   currentUserMessage.Text(),
		}
		if errAdd := c.addMessageToSQLite(ctx, dbUserMessage); errAdd != nil {
			log.Printf("Signal connector (ID: %s): Failed to add user message to SQLite history for %s: %v. Using current message only for context.", c.id, sender, errAdd)
			history = []provider.Message{currentUserMessage}
		} else {
			history, err = c.getHistoryFromSQLite(ctx, sender)
			if err != nil {
				log.Printf("Signal connector (ID: %s): Failed to get SQLite history for %s: %v. Using current message only for context.", c.id, sender, err)
				history = []provider.Message{currentUserMessage}
			}
		}
	} else {
		history, _ = c.conversationHistories[sender]
		history = append(history, currentUserMessage)
		if len(history) > c.maxHistoryMessages {
			startIndex := len(history) - c.maxHistoryMessages
			history = history[startIndex:]
			log.Printf("Signal connector (ID: %s): Memory history for %s truncated to last %d messages.", c.id, sender, c.maxHistoryMessages)
		}
		c.conversationHistories[sender] = history
	}

	log.Printf("Signal connector (ID: %s): Sending to LLM for sender %s (history length %d): %+v", c.id, sender, len(history), history)

	completeCtx, cancelComplete := context.WithTimeout(ctx, 30*time.Second)
	defer cancelComplete()

	llmResponse, err := c.completer.Complete(completeCtx, history, nil)
	if err != nil {
		log.Printf("Error from LLM completer for Signal message (ID: %s, sender: %s): %v", c.id, sender, err)
		return err
	}

	if llmResponse != nil && llmResponse.Message != nil && llmResponse.Message.Text() != "" {
		replyText := llmResponse.Message.Text()
		assistantMessage := *llmResponse.Message
		log.Printf("Signal connector (ID: %s): Received from LLM for sender %s: \"%s\"", c.id, sender, replyText)

		if c.historyStorageType == "sqlite" {
			dbAssistantMessage := dbMessage{
				ConversationID:   sender,
				MessageTimestamp: time.Now().UnixMilli(),
				MessageRole:      string(assistantMessage.Role),
				MessageContent:   assistantMessage.Text(),
			}
			if errAdd := c.addMessageToSQLite(ctx, dbAssistantMessage); errAdd != nil {
				log.Printf("Signal connector (ID: %s): Failed to add assistant message to SQLite history for %s: %v.", c.id, sender, errAdd)
			}
		} else {
			history = append(history, assistantMessage)
			if len(history) > c.maxHistoryMessages {
				startIndex := len(history) - c.maxHistoryMessages
				history = history[startIndex:]
				log.Printf("Signal connector (ID: %s): Memory history for %s (after assistant reply) truncated to last %d messages.", c.id, sender, c.maxHistoryMessages)
			}
			c.conversationHistories[sender] = history
		}

		if err := c.sendSignalMessage(ctx, sender, replyText); err != nil {
			log.Printf("Error sending Signal reply (ID: %s, to: %s): %v", c.id, sender, err)
			return err
		}
		log.Printf("Signal connector (ID: %s): Successfully sent reply to %s: \"%s\"", c.id, sender, replyText)
	} else {
		if llmResponse == nil || llmResponse.Message == nil {
			log.Printf("Signal connector (ID: %s): LLM response or message was nil for sender %s.", c.id, sender)
		} else {
			log.Printf("Signal connector (ID: %s): LLM provided empty reply for sender %s. Nothing to send.", c.id, sender)
		}
	}
	return nil
}

// dbMessage struct for SQLite interaction
type dbMessage struct {
	ConversationID   string `json:"conversation_id"`
	MessageTimestamp int64  `json:"message_timestamp"`
	MessageRole      string `json:"message_role"`
	MessageContent   string `json:"message_content"`
}

// getHistoryFromSQLite retrieves conversation history from SQLite
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
		// Convert roleStr to provider.MessageRole
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

// addMessageToSQLite adds a message to the SQLite history
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

	if resp.StatusCode != http.StatusCreated { // V2 Send expects 201 Created
		bodyBytes, _ := io.ReadAll(resp.Body)
		var sendErr SendMessageError
		if json.Unmarshal(bodyBytes, &sendErr) == nil && sendErr.Error != "" {
			return fmt.Errorf("failed to send message, status %d: %s", resp.StatusCode, sendErr.Error)
		}
		return fmt.Errorf("failed to send message, status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Optionally decode SendMessageV2Response if needed
	// var sendResp SendMessageV2Response
	// if err := json.NewDecoder(resp.Body).Decode(&sendResp); err != nil {
	// 	log.Printf("Warning: could not decode send response: %v", err)
	// }

	return nil
}
