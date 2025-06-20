package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

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
	if historyStorage != "memory" {
		return nil, fmt.Errorf("signal connector %s: invalid history_storage_type '%s'", id, cfg.HistoryStorageType)
	}
	log.Printf("Signal connector (ID: %s): Using history storage: %s", id, historyStorage)

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
		messagePrefixTrigger:  messagePrefixTrigger,
		llmTimeoutSeconds:     llmTimeout,
	}, nil
}

func (c *Connector) ID() string {
	return c.id
}

func (c *Connector) Start(ctx context.Context) error {
	log.Printf("Starting Signal connector (ID: %s), account: %s (username: '%s'), mode: %s, URL: %s, poll_interval: %v, history: %s, prefix: '%s'",
		c.id, c.accountNumber, c.accountUsername, c.receiveMode, c.url, c.pollInterval, c.historyStorageType, c.messagePrefixTrigger)

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

	// Print the fully received message as pretty JSON for inspection
	if msgJson, err := json.MarshalIndent(receivedMsg, "", "  "); err == nil {
		log.Printf("Signal connector (ID: %s) FULL RECEIVED MESSAGE:\n%s", c.id, string(msgJson))
	} else {
		log.Printf("Signal connector (ID: %s) failed to marshal receivedMsg for debug print: %v", c.id, err)
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

	history, _ = c.conversationHistories[conversationContextID]
	history = append(history, currentUserMessage)
	if len(history) > c.maxHistoryMessages {
		startIndex := len(history) - c.maxHistoryMessages
		history = history[startIndex:]
		log.Printf("Signal connector (ID: %s): Memory history for %s truncated to %d messages.", c.id, conversationContextID, c.maxHistoryMessages)
	}
	c.conversationHistories[conversationContextID] = history

	log.Printf("Signal connector (ID: %s): Sending to LLM for context %s (actual sender %s, history len %d): %+v", c.id, conversationContextID, actualSender, len(history), history)

	// Send typing indicator before starting LLM completion
	if receivedMsg.Envelope != nil && strings.HasPrefix(receivedMsg.Envelope.Source, "+") {
		if err := c.sendTypingIndicator(ctx, receivedMsg.Envelope.Source); err != nil {
			log.Printf("Signal connector (ID: %s): Failed to send typing indicator for %s: %v", c.id, receivedMsg.Envelope.Source, err)
		}
	} else if receivedMsg.Envelope != nil {
		log.Printf("Signal connector (ID: %s): Skipping typing indicator for non-phone-number recipient: %s", c.id, receivedMsg.Envelope.Source)
	}

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

		history = append(history, assistantMessage)
		if len(history) > c.maxHistoryMessages {
			startIndex := len(history) - c.maxHistoryMessages
			history = history[startIndex:]
			log.Printf("Signal connector (ID: %s): Memory history for %s (after reply) truncated to %d messages.", c.id, conversationContextID, c.maxHistoryMessages)
		}
		c.conversationHistories[conversationContextID] = history

		// Determine sender for the API call
		apiSender := c.accountNumber
		// For group replies, use accountUsername if set and contains a dot, else fallback to accountNumber
		if conversationContextID != actualSender && conversationContextID != c.accountNumber && c.accountUsername != "" && strings.Contains(c.accountUsername, ".") {
			apiSender = c.accountUsername
			log.Printf("Signal connector (ID: %s): Using account username '%s' as sender for group reply to %s", c.id, apiSender, conversationContextID)
		}

		// Send ready indicator before sending the reply
		if receivedMsg.Envelope != nil && strings.HasPrefix(receivedMsg.Envelope.Source, "+") {
			if err := c.sendReadyIndicator(ctx, receivedMsg.Envelope.Source); err != nil {
				log.Printf("Signal connector (ID: %s): Failed to send ready indicator for %s: %v", c.id, receivedMsg.Envelope.Source, err)
			}
		} else if receivedMsg.Envelope != nil {
			log.Printf("Signal connector (ID: %s): Skipping ready indicator for non-phone-number recipient: %s", c.id, receivedMsg.Envelope.Source)
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
		// Only send read receipt if actualSender looks like a phone number (starts with '+')
		if strings.HasPrefix(actualSender, "+") {
			if err := c.sendReadReceipt(ctx, actualSender, receivedMsg.Envelope.Timestamp); err != nil {
				log.Printf("Signal connector (ID: %s): Failed to send read receipt to API for %s at %d: %v", c.id, actualSender, receivedMsg.Envelope.Timestamp, err)
			} else {
				log.Printf("Signal connector (ID: %s): Sent read receipt to API for %s at %d", c.id, actualSender, receivedMsg.Envelope.Timestamp)
			}
		} else {
			log.Printf("Signal connector (ID: %s): Skipping read receipt for non-phone-number recipient: %s", c.id, actualSender)
		}
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

// sendTypingIndicator sends a PUT request to the typing indicator API to indicate typing.
func (c *Connector) sendTypingIndicator(ctx context.Context, phoneNumber string) error {
	escapedNumber := url.PathEscape(phoneNumber)
	apiURL := fmt.Sprintf("%s/v1/typing-indicator/%s", c.url, escapedNumber)
	body := map[string]interface{}{
		"recipient": phoneNumber,
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal typing indicator: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create PUT request for typing indicator: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to PUT typing indicator: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to send typing indicator, status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// sendReadyIndicator sends a DELETE request to the typing indicator API to indicate ready.
func (c *Connector) sendReadyIndicator(ctx context.Context, phoneNumber string) error {
	escapedNumber := url.PathEscape(phoneNumber)
	apiURL := fmt.Sprintf("%s/v1/typing-indicator/%s", c.url, escapedNumber)
	body := map[string]interface{}{
		"recipient": phoneNumber,
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal ready indicator: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create DELETE request for ready indicator: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to DELETE ready indicator: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to send ready indicator, status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}
