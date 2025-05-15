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
	// Default reconnect interval for WebSocket
	defaultReconnectInterval = 5 * time.Second
	maxReconnectInterval     = 60 * time.Second
)

type Connector struct {
	id                    string
	client                *http.Client
	url                   string
	accountNumber         string
	completer             provider.Completer
	pollInterval          time.Duration
	receiveMode           string              // "poll" or "websocket"
	whitelistedNumbersMap map[string]struct{} // For efficient lookup
	isWhitelistActive     bool
	conversationHistories map[string][]provider.Message
	maxHistoryMessages    int
}

// Config for the Signal connector
type Config struct {
	URL                string
	AccountNumber      string
	Completer          provider.Completer
	PollInterval       time.Duration
	ReceiveMode        string   // "poll" or "websocket"
	WhitelistedNumbers []string // New field for whitelisted numbers
	MaxHistoryMessages int      // Max messages (user + assistant) to keep
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
		cfg.PollInterval = 5 * time.Second // Default poll interval for poll mode
	}

	receiveMode := strings.ToLower(cfg.ReceiveMode)
	if receiveMode == "" {
		receiveMode = "poll" // Default to poll if not specified
	}

	if receiveMode != "poll" && receiveMode != "websocket" {
		return nil, fmt.Errorf("signal connector %s: invalid receive_mode '%s', must be 'poll' or 'websocket'", id, cfg.ReceiveMode)
	}

	if receiveMode == "websocket" && cfg.URL == "" {
		// The URL is used to derive the WebSocket URL.
		// For polling, it's also directly used.
		return nil, fmt.Errorf("signal connector %s: url is required", id)
	}

	whitelistedMap := make(map[string]struct{})
	isWhitelistActive := false
	if len(cfg.WhitelistedNumbers) > 0 {
		isWhitelistActive = true
		for _, num := range cfg.WhitelistedNumbers {
			// Normalize numbers? For now, assume they are provided in a consistent format (e.g., with +countrycode)
			if num != "" {
				whitelistedMap[num] = struct{}{}
			}
		}
		log.Printf("Signal connector (ID: %s): Whitelist active. Allowed numbers: %v", id, cfg.WhitelistedNumbers)
	} else {
		log.Printf("Signal connector (ID: %s): Whitelist is empty. Defaulting to 'Note to Self' only mode.", id)
	}

	maxHistory := cfg.MaxHistoryMessages
	if maxHistory <= 0 {
		maxHistory = 20 // Default to keeping last 20 messages (10 turns)
		log.Printf("Signal connector (ID: %s): MaxHistoryMessages not set or invalid, defaulting to %d", id, maxHistory)
	}

	return &Connector{
		id:                    id,
		client:                http.DefaultClient, // HTTP client still needed for sending replies
		url:                   strings.TrimSuffix(cfg.URL, "/"),
		accountNumber:         cfg.AccountNumber,
		completer:             cfg.Completer,
		pollInterval:          cfg.PollInterval, // Still stored, used if mode is poll
		receiveMode:           receiveMode,
		whitelistedNumbersMap: whitelistedMap,
		isWhitelistActive:     isWhitelistActive,
		conversationHistories: make(map[string][]provider.Message),
		maxHistoryMessages:    maxHistory,
	}, nil
}

func (c *Connector) ID() string {
	return c.id
}

func (c *Connector) Start(ctx context.Context) error {
	log.Printf("Starting Signal connector (ID: %s), account: %s, mode: %s, URL: %s, poll_interval: %v", c.id, c.accountNumber, c.receiveMode, c.url, c.pollInterval)

	if c.receiveMode == "websocket" {
		return c.runWebSocketListener(ctx)
	}

	// Default to polling mode
	log.Printf("Signal connector (ID: %s): Using POLL mode.", c.id)
	log.Printf("Signal connector (ID: %s): About to create ticker for POLL mode. Interval value: %v (type: %T)", c.id, c.pollInterval, c.pollInterval)
	ticker := time.NewTicker(c.pollInterval)
	log.Printf("Signal connector (ID: %s): Ticker creation attempted for POLL mode. Ticker object: %p", c.id, ticker)

	defer ticker.Stop()
	log.Printf("Signal connector (ID: %s): Defer ticker.Stop() registered for POLL mode.", c.id)

	log.Printf("Signal connector (ID: %s): Successfully created ticker for POLL mode. Entering main select loop...", c.id)

	for {
		// log.Printf("Signal connector (ID: %s): Top of select loop (POLL mode), waiting for tick or context cancellation...", c.id) // Optional: very verbose
		select {
		case <-ctx.Done():
			log.Printf("Stopping Signal connector (ID: %s) (POLL mode): context cancelled: %v", c.id, ctx.Err())
			return ctx.Err()
		case tickTime := <-ticker.C:
			log.Printf("Signal connector (ID: %s) (POLL mode): Tick received at %v, attempting to poll for messages...", c.id, tickTime)
			if err := c.pollAndProcessMessages(ctx); err != nil {
				log.Printf("Error polling Signal messages (ID: %s) (POLL mode): %v", c.id, err)
				// Decide if error is fatal or if we should continue polling
			} else {
				// Optionally, log successful poll even if no messages were found.
				// pollAndProcessMessages logs if messages > 0.
				// If it returns nil and no messages were logged by it, it means 0 messages.
				// log.Printf("Signal connector (ID: %s): Polled successfully (0 messages or processed).", c.id) // Uncomment if more verbosity is desired
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
		// It's possible the body is empty if no messages, which is not an error for json.Decoder
		// if it's an actual JSON parsing error for non-empty body, then it's an issue.
		bodyBytes, _ := io.ReadAll(resp.Body) // Re-read for logging if decode failed.
		if len(bodyBytes) > 0 {
			return fmt.Errorf("failed to decode received message strings: %w. Body: %s", err, string(bodyBytes))
		}
		// If body was empty, no messages, just return.
		return nil
	}

	if len(messageStrings) == 0 {
		return nil // No new messages
	}

	log.Printf("Signal connector (ID: %s) (POLL mode) received %d raw message strings/events.", c.id, len(messageStrings))

	for _, msgStr := range messageStrings {
		// Use a common processing function
		if err := c.processReceivedMessagePayload(ctx, []byte(msgStr)); err != nil {
			log.Printf("Signal connector (ID: %s) (POLL mode) error processing message payload: %v. Raw string: %s", c.id, err, msgStr)
			// Decide if we should continue with other messages or return the error
		}
	}
	return nil
}

// runWebSocketListener handles the WebSocket connection and message listening.
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

	// Construct WebSocket URL: ws(s)://host:port/v1/receive/accountNumber
	wsURL := url.URL{
		Scheme: wsScheme,
		Host:   httpBaseURL.Host, // Takes host and port from the original http URL
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
		currentReconnectDelay = defaultReconnectInterval // Reset reconnect delay on successful connection

		// Ensure connection is closed when the function exits or the read loop breaks
		connClosed := make(chan struct{})
		defer func() {
			// This defer will run when runWebSocketListener exits.
			// We also need to handle closing if the read loop breaks before context is done.
			select {
			case <-connClosed: // Already closed by the read loop
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
			case <-connClosed: // If already closed by read loop error
				return
			}
		}()

		for { // Inner loop for reading messages
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Error reading message: %v. Will attempt to reconnect.", c.id, err)
				// Check if the error is a normal closure or an unexpected one
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
					// Log more serious errors
				}
				conn.Close()      // Ensure connection is closed before trying to reconnect
				close(connClosed) // Signal that connection was closed by read loop
				break             // Break inner loop to trigger reconnection
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
				// gorilla/websocket handles responding to pings automatically by default
				// If custom pong handling is needed: err = conn.WriteMessage(websocket.PongMessage, []byte{})
			case websocket.PongMessage:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received pong.", c.id)
			case websocket.CloseMessage:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received close message. Will attempt to reconnect.", c.id)
				conn.Close()
				close(connClosed)
				break // Break inner loop
			default:
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode) received unknown message type: %d", c.id, messageType)
			}

			// Check context after processing a message, to allow faster shutdown if needed
			select {
			case <-ctx.Done():
				log.Printf("Signal connector (ID: %s) (WEBSOCKET mode): Context cancelled during message processing. Listener stopping.", c.id)
				// conn.Close() is handled by defer and the goroutine watching ctx.Done()
				return ctx.Err()
			default:
			}
		} // End of inner message read loop

		// If we broke from inner loop due to read error/close, outer loop will retry connection
	} // End of outer connection loop (unreachable due to for {} without break for ctx.Done() directly in it, but ctx.Done() in select handles exit)
}

// processReceivedMessagePayload unmarshals and processes a single message payload.
func (c *Connector) processReceivedMessagePayload(ctx context.Context, payload []byte) error {
	log.Printf("Signal connector (ID: %s) processing raw payload: %s", c.id, string(payload))

	var receivedMsg ReceivedSignalMessage
	if err := json.Unmarshal(payload, &receivedMsg); err != nil {
		log.Printf("Error unmarshalling Signal message payload (ID: %s): %v. Raw payload: %s", c.id, err, string(payload))
		// Potentially handle as raw text if unmarshal fails but string is not empty
		if len(payload) > 0 {
			// This part needs careful thought: if it's not JSON, what is it?
			// For now, we'll assume it should be JSON.
			return fmt.Errorf("failed to unmarshal non-empty payload: %w", err)
		}
		log.Printf("Signal connector (ID: %s) received empty payload, skipping.", c.id)
		return nil // Or an error indicating empty payload if that's unexpected
	}

	log.Printf("Signal connector (ID: %s) successfully unmarshalled payload. Account: %s, Envelope: %+v, CallMessage: %+v", c.id, receivedMsg.Account, receivedMsg.Envelope, receivedMsg.CallMessage) // Removed DataMessage from top level log as it's in Envelope

	// Process the actual message content
	var sender, messageText string
	var isNoteToSelf, isDataMessageFromOther bool

	if receivedMsg.Envelope != nil {
		// Check for "Note to Self" via SentMessage sync
		if receivedMsg.Envelope.SyncMessage != nil &&
			receivedMsg.Envelope.SyncMessage.SentMessage != nil &&
			receivedMsg.Envelope.SyncMessage.SentMessage.Message != "" &&
			receivedMsg.Envelope.Source == c.accountNumber &&
			(receivedMsg.Envelope.SyncMessage.SentMessage.Destination == c.accountNumber || receivedMsg.Envelope.SyncMessage.SentMessage.DestinationNumber == c.accountNumber) {

			sender = receivedMsg.Envelope.Source
			messageText = receivedMsg.Envelope.SyncMessage.SentMessage.Message
			isNoteToSelf = true
			log.Printf("Signal connector (ID: %s): Identified 'Note to Self' (SentMessage sync) from %s. Text: \"%s\"", c.id, sender, messageText)

			// Check for standard incoming DataMessage
		} else if receivedMsg.Envelope.DataMessage != nil && receivedMsg.Envelope.DataMessage.Message != "" {
			if receivedMsg.Envelope.Source != "" {
				sender = receivedMsg.Envelope.Source
				messageText = receivedMsg.Envelope.DataMessage.Message
				if sender != c.accountNumber {
					isDataMessageFromOther = true
					// Whitelist check will happen below
				} else {
					// DataMessage from self to someone else, or echo. Not a "Note to Self" for LLM processing.
					log.Printf("Signal connector (ID: %s): Ignoring incoming DataMessage originating from self to other(s) (%s). Text: \"%s\"", c.id, sender, messageText)
				}
			} else {
				log.Printf("Warning: Signal DataMessage (ID: %s) received without clear sender in envelope. DataMessage: %+v", c.id, receivedMsg.Envelope.DataMessage)
			}
		}
	}

	shouldProcessForLLM := false
	if isNoteToSelf {
		shouldProcessForLLM = true // Always process "Note to Self"
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
		// Log if it was some other kind of message we didn't explicitly handle or ignore above
		if sender == "" && messageText == "" && (receivedMsg.Envelope == nil || (receivedMsg.Envelope.DataMessage == nil && receivedMsg.Envelope.SyncMessage == nil && receivedMsg.Envelope.ReceiptMessage == nil)) && receivedMsg.CallMessage == nil {
			log.Printf("Signal connector (ID: %s): Received unhandled or non-text event type. Payload: %s", c.id, string(payload))
		}
		return nil
	}

	// At this point, sender and messageText should be populated if shouldProcessForLLM is true.
	if sender == "" || messageText == "" { // Should not happen if shouldProcessForLLM is true
		log.Printf("Signal connector (ID: %s): Internal logic error - sender or messageText empty despite shouldProcessForLLM=true. Payload: %s", c.id, string(payload))
		return nil
	}

	log.Printf("Signal connector (ID: %s): Preparing to send to LLM. Sender: %s, Message: \"%s\"", c.id, sender, messageText)

	// Retrieve or initialize conversation history
	history, _ := c.conversationHistories[sender] // ok is false if sender is new

	// Add current user message to history
	history = append(history, provider.UserMessage(messageText))

	// Truncate history if it exceeds max length
	if len(history) > c.maxHistoryMessages {
		startIndex := len(history) - c.maxHistoryMessages
		history = history[startIndex:]
		log.Printf("Signal connector (ID: %s): History for %s truncated to last %d messages.", c.id, sender, c.maxHistoryMessages)
	}

	log.Printf("Signal connector (ID: %s): Sending to LLM for sender %s (history length %d): %+v", c.id, sender, len(history), history)

	completeCtx, cancelComplete := context.WithTimeout(ctx, 30*time.Second)
	defer cancelComplete()

	llmResponse, err := c.completer.Complete(completeCtx, history, nil)
	if err != nil {
		log.Printf("Error from LLM completer for Signal message (ID: %s, sender: %s): %v", c.id, sender, err)
		// Don't save user message if LLM fails, to avoid cluttering history with unreplied messages? Or save it?
		// For now, we'll save it, as it was part of the attempt.
		c.conversationHistories[sender] = history
		return err // Propagate LLM error
	}

	if llmResponse != nil && llmResponse.Message != nil && llmResponse.Message.Text() != "" {
		replyText := llmResponse.Message.Text()
		log.Printf("Signal connector (ID: %s): Received from LLM for sender %s: \"%s\"", c.id, sender, replyText)

		// Add assistant's reply to history
		history = append(history, *llmResponse.Message) // llmResponse.Message is a pointer

		// Truncate history again if it exceeds max length after adding assistant's reply
		if len(history) > c.maxHistoryMessages {
			startIndex := len(history) - c.maxHistoryMessages
			history = history[startIndex:]
			log.Printf("Signal connector (ID: %s): History for %s (after assistant reply) truncated to last %d messages.", c.id, sender, c.maxHistoryMessages)
		}
		c.conversationHistories[sender] = history // Save updated history

		if err := c.sendSignalMessage(ctx, sender, replyText); err != nil {
			log.Printf("Error sending Signal reply (ID: %s, to: %s): %v", c.id, sender, err)
			return err // Propagate send error
		}
		log.Printf("Signal connector (ID: %s): Successfully sent reply to %s: \"%s\"", c.id, sender, replyText)

	} else {
		if llmResponse == nil || llmResponse.Message == nil {
			log.Printf("Signal connector (ID: %s): LLM response or message was nil for sender %s.", c.id, sender)
		} else { // llmResponse.Message.Text() is empty
			log.Printf("Signal connector (ID: %s): LLM provided empty reply for sender %s. Nothing to send.", c.id, sender)
		}
		// Save history even if LLM response was empty/nil, as user message was processed
		c.conversationHistories[sender] = history
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
