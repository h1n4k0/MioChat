package main

import "time"

const (
	attachmentCapability                = "attachments-v1"
	attachmentReferenceCapability       = "attachment-references-v1"
	attachmentDailyLimit          int64 = 100000000
	attachmentItemLimit           int64 = 20000000
	attachmentChunkLimit                = 24 * 1024
	attachmentPendingLimit              = 4
	attachmentTTL                       = 30 * time.Minute
)

// Bytes are source-file bytes, not JSON/base64. Decimal units and UTC+08:00
// midnight are implementation assumptions, not additional user requirements.
// Accepted bytes stay charged on cancel/failure; only unreceived reservations
// are released. Clients must disclose this before the first intent.
type attachmentQuota struct {
	Day        string `json:"day"`
	Used       int64  `json:"used"`
	Reserved   int64  `json:"reserved"`
	DailyLimit int64  `json:"daily_limit"`
	ItemLimit  int64  `json:"item_limit"`
	ResetAt    int64  `json:"reset_at"`
	Meter      string `json:"meter"`
}

type attachmentInfo struct {
	ID             string `json:"id"`
	Filename       string `json:"filename"`
	MIME           string `json:"mime"`
	Size           int64  `json:"size"`
	SHA256         string `json:"sha256"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	Frames         int    `json:"frames,omitempty"`
	MetadataPolicy string `json:"metadata_policy"`
}

// One typed request per <=64KiB JSON line; no arbitrary paths, URLs or inline
// data URLs. A client obtains this capability from hello before sending any.
// intent: key/filename/mime/size/sha256/caption. chunk: upload_id/offset/data_b64.
// commit/cancel: upload_id. status: exactly one of upload_id or intent_key.
// fetch: message_id/offset (returns at most 24KiB). limits: no extra fields.
// Room is the outer guiRequest.Room and MUST equal the current GUI scope.
type guiAttachmentRequest struct {
	Op        string `json:"op"`
	IntentKey string `json:"intent_key,omitempty"`
	UploadID  string `json:"upload_id,omitempty"`
	Filename  string `json:"filename,omitempty"`
	MIME      string `json:"mime,omitempty"`
	Size      int64  `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Caption   string `json:"caption,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	DataB64   string `json:"data_b64,omitempty"`
	MessageID int64  `json:"message_id,omitempty"`
}

// status: uploading, committed, cancelled, expired, rejected, unknown, limits,
// or fetched. "unknown" is not failure/rollback: query the SAME intent/upload;
// never retry with a new intent automatically. Error code is stable and contains
// no SQL, paths, credentials, or file bytes. Successful commit may be replayed.
type guiAttachmentReceipt struct {
	Op              string           `json:"op,omitempty"`
	Status          string           `json:"status"`
	Code            string           `json:"code,omitempty"`
	IntentKey       string           `json:"intent_key,omitempty"`
	UploadID        string           `json:"upload_id,omitempty"`
	Room            string           `json:"room"`
	OwnerUID        int64            `json:"owner_uid,omitempty"`
	AcceptedOffset  int64            `json:"accepted_offset"`
	Size            int64            `json:"size,omitempty"`
	ExpiresAt       int64            `json:"expires_at,omitempty"`
	MessageID       int64            `json:"message_id,omitempty"`
	SourceMessageID int64            `json:"source_message_id,omitempty"`
	Attachment      *attachmentInfo  `json:"attachment,omitempty"`
	Quota           *attachmentQuota `json:"quota,omitempty"`
	Offset          int64            `json:"offset,omitempty"`
	DataB64         string           `json:"data_b64,omitempty"`
	More            bool             `json:"more,omitempty"`
	ChunkLimit      int              `json:"chunk_limit,omitempty"`
	AllowedMIMEs    []string         `json:"allowed_mimes,omitempty"`
	NewCommit       bool             `json:"-"`
}
