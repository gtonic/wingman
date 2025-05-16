package signal

// SendMessageV2Request is based on #/definitions/api.SendMessageV2
type SendMessageV2Request struct {
	Number            string   `json:"number"` // Sender's Signal account number
	Recipients        []string `json:"recipients"`
	Message           string   `json:"message"`
	Base64Attachments []string `json:"base64_attachments,omitempty"`
	TextMode          string   `json:"text_mode,omitempty"` // "normal" or "styled"
	Mentions          []any    `json:"mentions,omitempty"`  // Define more specifically if needed
	QuoteTimestamp    int64    `json:"quote_timestamp,omitempty"`
	QuoteAuthor       string   `json:"quote_author,omitempty"`
	QuoteMessage      string   `json:"quote_message,omitempty"`
	QuoteMentions     []any    `json:"quote_mentions,omitempty"` // Define more specifically if needed
	EditTimestamp     int64    `json:"edit_timestamp,omitempty"`
	LinkPreview       any      `json:"link_preview,omitempty"` // Define more specifically if needed
	NotifySelf        bool     `json:"notify_self,omitempty"`
	Sticker           string   `json:"sticker,omitempty"`
}

// SendMessageV2Response is based on #/definitions/api.SendMessageResponse
type SendMessageV2Response struct {
	Timestamp string `json:"timestamp"`
	// Potentially other fields if the API returns more on success
}

// SendMessageError is based on #/definitions/api.SendMessageError
type SendMessageError struct {
	Error           string   `json:"error"`
	Account         string   `json:"account,omitempty"`
	ChallengeTokens []string `json:"challenge_tokens,omitempty"`
}

// ReceivedSignalMessage represents the expected structure of a message
// received from the GET /v1/receive/{number} endpoint.
// ReceivedSignalMessage attempts to map the structure observed from user feedback.
// The Signal CLI REST API might have various event types; this tries to capture common ones.
type ReceivedSignalMessage struct {
	Account  string    `json:"account,omitempty"`  // Top-level account this message pertains to
	Envelope *Envelope `json:"envelope,omitempty"` // Envelope now contains more details

	// DataMessage is for actual text/content messages.
	// Its structure might vary based on the Signal CLI API version.
	// DataMessage is now part of the Envelope, as observed in user's payload
	// DataMessage *DataMessage `json:"dataMessage,omitempty"` // No longer top-level

	// CallMessage for call-related events. (Its position relative to envelope vs. top-level TBD if a payload is seen)
	CallMessage *CallMessage `json:"callMessage,omitempty"`

	// StoryMessage for story-related events (if applicable and different from DataMessage).
	// StoryMessage *StoryMessage `json:"storyMessage,omitempty"` // Example if stories have a distinct structure

	// Other top-level fields might exist for different event types.
	// For example, some APIs might have a top-level 'type' field indicating event type.

	RawMessage string `json:"-"` // Used if unmarshalling into specific structs fails or for unhandled parts
}

type Envelope struct {
	Source                   string `json:"source,omitempty"` // Sender's number or group ID
	SourceNumber             string `json:"sourceNumber,omitempty"`
	SourceUuid               string `json:"sourceUuid,omitempty"`
	SourceName               string `json:"sourceName,omitempty"`
	SourceDevice             int    `json:"sourceDevice,omitempty"`
	Timestamp                int64  `json:"timestamp,omitempty"` // Original message timestamp
	ServerReceivedTimestamp  int64  `json:"serverReceivedTimestamp,omitempty"`
	ServerDeliveredTimestamp int64  `json:"serverDeliveredTimestamp,omitempty"`
	IsReceipt                bool   `json:"isReceipt,omitempty"` // General receipt flag
	Relay                    string `json:"relay,omitempty"`     // Relay server

	// SyncMessage is now nested within Envelope as per user's payload
	SyncMessage *SyncMessage `json:"syncMessage,omitempty"`

	// ReceiptMessage is also nested within Envelope as per user's latest payload
	ReceiptMessage *ReceiptMessage `json:"receiptMessage,omitempty"`

	// DataMessage is now nested within Envelope
	DataMessage *DataMessage `json:"dataMessage,omitempty"`
}

type ReceiptMessage struct {
	When       int64   `json:"when,omitempty"`
	IsDelivery bool    `json:"isDelivery,omitempty"`
	IsRead     bool    `json:"isRead,omitempty"`
	IsViewed   bool    `json:"isViewed,omitempty"`
	Timestamps []int64 `json:"timestamps,omitempty"`
}

type DataMessage struct {
	Timestamp        int64        `json:"timestamp,omitempty"`
	Message          string       `json:"message,omitempty"` // The actual text content
	ExpiresInSeconds uint32       `json:"expiresInSeconds,omitempty"`
	ViewOnce         bool         `json:"viewOnce,omitempty"`
	Mentions         []any        `json:"mentions,omitempty"` // Define more specifically if structure is known
	Quote            *Quote       `json:"quote,omitempty"`
	GroupInfo        *GroupInfo   `json:"groupInfo,omitempty"`
	Reaction         *Reaction    `json:"reaction,omitempty"` // For emoji reactions
	Sticker          *Sticker     `json:"sticker,omitempty"`
	Attachments      []Attachment `json:"attachments,omitempty"`
}

type SyncMessage struct {
	SentMessages          []SentMessageReceipt `json:"sentMessages,omitempty"`          // Confirmation of messages sent by this account
	ReadMessages          []ReadMessageReceipt `json:"readMessages,omitempty"`          // Confirmation of messages read by others
	ViewedMessages        []ViewedReceipt      `json:"viewedMessages,omitempty"`        // For viewed status (e.g. stories)
	BlockedNumbers        []BlockedContact     `json:"blockedNumbers,omitempty"`        // List of blocked numbers/groups
	Contacts              *ContactsSync        `json:"contacts,omitempty"`              // Full contacts sync
	Groups                *GroupsSync          `json:"groups,omitempty"`                // Group list sync
	Configuration         *ConfigurationSync   `json:"configuration,omitempty"`         // Sync of configuration options
	FetchType             string               `json:"fetchType,omitempty"`             // e.g., "CONTACTS_SYNC"
	StickerPackOperations []any                `json:"stickerPackOperations,omitempty"` // Define if structure is known
	SentMessage           *SentMessagePayload  `json:"sentMessage,omitempty"`           // For "Note to Self" / sent message sync
	// Other sync types can be added here
}

type SentMessagePayload struct {
	Destination        string       `json:"destination,omitempty"`
	DestinationNumber  string       `json:"destinationNumber,omitempty"`
	DestinationUuid    string       `json:"destinationUuid,omitempty"`
	Timestamp          int64        `json:"timestamp,omitempty"`
	Message            string       `json:"message,omitempty"` // The actual message text
	ExpiresInSeconds   int          `json:"expiresInSeconds,omitempty"`
	ViewOnce           bool         `json:"viewOnce,omitempty"`
	DistributionListId string       `json:"distributionListId,omitempty"`
	Mentions           []any        `json:"mentions,omitempty"`
	Attachments        []Attachment `json:"attachments,omitempty"`
	Quote              *Quote       `json:"quote,omitempty"`
	GroupInfo          *GroupInfo   `json:"groupInfo,omitempty"` // If sent to a group
	// Other fields like reactions, stickers if they can be part of a sent message sync
}

type SentMessageReceipt struct { // This is for delivery/read receipts FOR a sent message, not the sent message itself
	Destination string `json:"destination,omitempty"`
	Timestamp   int64  `json:"timestamp,omitempty"`
	// MessageRange []any  `json:"messageRange,omitempty"` // Define if structure is known
}

type ReadMessageReceipt struct {
	Sender       string `json:"sender,omitempty"` // Who sent the original message that was read
	SenderNumber string `json:"senderNumber,omitempty"`
	SenderUuid   string `json:"senderUuid,omitempty"`
	Timestamp    int64  `json:"timestamp,omitempty"` // Timestamp of the original message that was read
}

type ViewedReceipt struct {
	Sender    string `json:"sender,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

type BlockedContact struct {
	Number string `json:"number,omitempty"`
	Name   string `json:"name,omitempty"`
}

type ContactsSync struct {
	// Define structure if known, often a list of contact details
}

type GroupsSync struct {
	// Define structure if known, often a list of group details
}

type ConfigurationSync struct {
	ReadReceipts *bool `json:"readReceipts,omitempty"`
	// Other configuration items
}

type CallMessage struct {
	OfferMessage     *CallOffer      `json:"offerMessage,omitempty"`
	AnswerMessage    *CallAnswer     `json:"answerMessage,omitempty"`
	IceUpdateMessage []CallIceUpdate `json:"iceUpdateMessage,omitempty"`
	BusyMessage      *CallBusy       `json:"busyMessage,omitempty"`
	HangupMessage    *CallHangup     `json:"hangupMessage,omitempty"`
}

type CallOffer struct {
	ID          string `json:"id,omitempty"`
	Description string `json:"description,omitempty"` // SDP offer
	Timestamp   int64  `json:"timestamp,omitempty"`
}

type CallAnswer struct {
	ID          string `json:"id,omitempty"`
	Description string `json:"description,omitempty"` // SDP answer
	Timestamp   int64  `json:"timestamp,omitempty"`
}

type CallIceUpdate struct {
	ID            string `json:"id,omitempty"`
	SdpMid        string `json:"sdpMid,omitempty"`
	SdpMLineIndex int    `json:"sdpMLineIndex,omitempty"`
	Sdp           string `json:"sdp,omitempty"` // ICE candidate
	Timestamp     int64  `json:"timestamp,omitempty"`
}

type CallBusy struct {
	ID        string `json:"id,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

type CallHangup struct {
	ID        string `json:"id,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

type Quote struct {
	ID     int64  `json:"id,omitempty"` // Timestamp of the quoted message
	Author string `json:"author,omitempty"`
	Text   string `json:"text,omitempty"`
	// Mentions, attachments etc. if quotes can have them
}

type GroupInfo struct {
	GroupID string `json:"groupId,omitempty"`
	Type    string `json:"type,omitempty"` // e.g., DELIVER, UPDATE, QUIT
	// Name, Members, Avatar etc. for group updates
}

type Reaction struct {
	Emoji               string `json:"emoji,omitempty"`
	TargetAuthor        string `json:"targetAuthor,omitempty"`        // Author of the message being reacted to
	TargetSentTimestamp int64  `json:"targetSentTimestamp,omitempty"` // Timestamp of the message being reacted to
	Remove              bool   `json:"remove,omitempty"`              // True if this is a reaction removal
}

type Sticker struct {
	PackID    string `json:"packId,omitempty"`
	StickerID int    `json:"stickerId,omitempty"`
	// other sticker details
}

type Attachment struct {
	ContentType string `json:"contentType,omitempty"`
	Filename    string `json:"filename,omitempty"`
	ID          string `json:"id,omitempty"` // Attachment ID for later retrieval
	Size        int64  `json:"size,omitempty"`
	// Other fields like width, height, duration for media
}
