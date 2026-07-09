package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// whatsappDataDir resolves the out-of-tree directory that holds the WhatsApp
// session DB, the message DB, and downloaded media. Everything sensitive lives
// under ~/.config/homebase/whatsapp (created 0700, owner-only) — NEVER inside
// the repo tree. Go does not expand "~", so the home dir is resolved explicitly.
func whatsappDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home directory: %v", err)
	}
	dir := filepath.Join(home, ".config", "homebase", "whatsapp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed to create data directory %s: %v", dir, err)
	}
	return dir, nil
}

// enforceMode600 REFUSES TO START if path exists and is not EXACTLY mode 0600.
// A world/group-readable session/message DB is a custody failure — the session
// DB holds the linked-device credentials — so we log a clear error and exit(1)
// before connecting. A missing file is fine (first run). Mirrors
// enforce_mode_600 in scripts/mcp/drive-user-mcp/server.py.
func enforceMode600(path, what string, logger waLog.Logger) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return // first run — nothing to enforce yet
		}
		logger.Errorf("REFUSING TO START: cannot stat %s at %s: %v", what, path, err)
		os.Exit(1)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		logger.Errorf("REFUSING TO START: %s at %s is mode %04o, must be exactly 0600 "+
			"(owner read/write only, no group/other). Fix it: chmod 600 %q", what, path, mode, path)
		os.Exit(1)
	}
}

// sendTokenPath resolves the file that holds the bearer token gating /api/send.
// It lives beside the databases under ~/.config/homebase/whatsapp so the whole
// custody surface is one owner-only (0700) directory.
func sendTokenPath() (string, error) {
	dir, err := whatsappDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "send-token"), nil
}

// loadOrCreateSendToken returns the exact bearer token that every /api/send
// request must present. On first run it generates a cryptographically-random
// 256-bit token and writes it 0600. On subsequent runs it reads the existing
// token but REFUSES TO START if the file is not EXACTLY mode 0600 — a
// group/world-readable send token would let ANY local process drive the linked
// WhatsApp account, which is the very thing loopback-binding + this token exist
// to prevent, so a custody failure halts the bridge (mirrors enforceMode600 for
// the session/message DBs). This function is the ONLY writer of the token; the
// cockpit send-gate is the only intended reader.
func loadOrCreateSendToken(logger waLog.Logger) string {
	path, err := sendTokenPath()
	if err != nil {
		logger.Errorf("REFUSING TO START: %v", err)
		os.Exit(1)
	}

	info, statErr := os.Stat(path)
	if statErr == nil {
		// Existing token — enforce exact 0600 before trusting it.
		if mode := info.Mode().Perm(); mode != 0o600 {
			logger.Errorf("REFUSING TO START: send token at %s is mode %04o, must be exactly 0600 "+
				"(owner read/write only, no group/other). It may be compromised — regenerate it: "+
				"rm %q && restart. Or fix perms: chmod 600 %q", path, mode, path, path)
			os.Exit(1)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			logger.Errorf("REFUSING TO START: cannot read send token at %s: %v", path, err)
			os.Exit(1)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			logger.Errorf("REFUSING TO START: send token at %s is empty; delete it to regenerate", path)
			os.Exit(1)
		}
		logger.Infof("Loaded /api/send bearer token from %s (mode 0600)", path)
		return token
	}
	if !os.IsNotExist(statErr) {
		logger.Errorf("REFUSING TO START: cannot stat send token at %s: %v", path, statErr)
		os.Exit(1)
	}

	// First run — generate a fresh 256-bit token and persist it owner-only.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		logger.Errorf("REFUSING TO START: failed to generate send token: %v", err)
		os.Exit(1)
	}
	token := hex.EncodeToString(raw)

	// O_EXCL: never clobber a token that raced in between the stat and now.
	// The 0600 create mode plus the explicit Chmod below guarantee exactly 0600
	// regardless of the process umask.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		logger.Errorf("REFUSING TO START: failed to create send token at %s: %v", path, err)
		os.Exit(1)
	}
	if _, err := f.WriteString(token); err != nil {
		f.Close()
		logger.Errorf("REFUSING TO START: failed to write send token at %s: %v", path, err)
		os.Exit(1)
	}
	if err := f.Close(); err != nil {
		logger.Errorf("REFUSING TO START: failed to close send token at %s: %v", path, err)
		os.Exit(1)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		logger.Errorf("REFUSING TO START: failed to secure send token at %s: %v", path, err)
		os.Exit(1)
	}
	logger.Infof("Generated new /api/send bearer token at %s (mode 0600)", path)
	return token
}

// presentedSendToken pulls the caller's token from the request: either an
// "Authorization: Bearer <token>" header or an "X-Send-Token: <token>" header.
// Returns "" when neither is present.
func presentedSendToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if rest, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Send-Token"))
}

// Initialize message store at the given (out-of-tree) path. The DB file is
// chmod'd to 0600 after creation so message history stays owner-only.
func NewMessageStore(dbPath string) (*MessageStore, error) {
	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			direct_path TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Forward-only additive migration: message DBs created before the
	// direct_path column predate the authenticated-download fix. WhatsApp's
	// current media download (whatsmeow >= 2026-05-31, which dropped legacy
	// direct-URL support) requires the proto DirectPath — the path plus its
	// signed ?...&oh=&oe=&_nc_sid query — not the stored CDN URL. Add the
	// column to legacy DBs so newly received messages can persist it.
	if err := ensureDirectPathColumn(db); err != nil {
		db.Close()
		return nil, err
	}

	// Lock the message DB down to owner-only (SQLite creates it at umask perms).
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to secure message database permissions: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// ensureDirectPathColumn adds the messages.direct_path column to older
// databases that predate it. It is idempotent: SQLite's ALTER TABLE ADD COLUMN
// errors if the column already exists, so we check PRAGMA table_info first and
// only add when absent.
func ensureDirectPathColumn(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(messages)")
	if err != nil {
		return fmt.Errorf("failed to inspect messages schema: %v", err)
	}
	defer rows.Close()

	hasDirectPath := false
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notnull, pk int
			dfltValue   sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("failed to scan messages schema: %v", err)
		}
		if name == "direct_path" {
			hasDirectPath = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read messages schema: %v", err)
	}

	if !hasDirectPath {
		if _, err := db.Exec("ALTER TABLE messages ADD COLUMN direct_path TEXT"); err != nil {
			return fmt.Errorf("failed to add direct_path column: %v", err)
		}
	}
	return nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

// SendMessageRequest is the /api/send request body. Text messages ONLY for now
// (HMB-333 Phase 2); media send is deliberately out of scope until a later phase.
type SendMessageRequest struct {
	RecipientJID string `json:"recipient_jid"`
	Text         string `json:"text"`
}

// SendMessageResponse is the /api/send result. MessageID is the whatsmeow-assigned
// id of the sent message, present only on success.
type SendMessageResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	MessageID string `json:"message_id,omitempty"`
}

// sendTextMessage sends a plain-text WhatsApp message via whatsmeow's
// SendMessage. Text only — media is intentionally unsupported here (Phase 2
// scope). Returns (ok, messageID, humanMessage).
func sendTextMessage(ctx context.Context, client *whatsmeow.Client, recipientJID, text string) (bool, string, string) {
	if !client.IsConnected() {
		return false, "", "Not connected to WhatsApp"
	}

	var jid types.JID
	var err error
	if strings.Contains(recipientJID, "@") {
		jid, err = types.ParseJID(recipientJID)
		if err != nil {
			return false, "", fmt.Sprintf("Invalid recipient JID: %v", err)
		}
	} else {
		// Bare phone number → personal chat JID.
		jid = types.JID{User: recipientJID, Server: "s.whatsapp.net"}
	}

	// Conversation is a *string; take the address of the local copy so we avoid
	// pulling in google.golang.org/protobuf just for proto.String.
	msg := &waProto.Message{Conversation: &text}
	resp, err := client.SendMessage(ctx, jid, msg)
	if err != nil {
		return false, "", fmt.Sprintf("Error sending message: %v", err)
	}
	return true, resp.ID, fmt.Sprintf("Message sent to %s", jid.String())
}

// Extract media info from a message. DirectPath is captured alongside URL: as
// of whatsmeow's 2026-05-31 removal of legacy direct-URL downloads, the
// authenticated download is derived from the DirectPath (path + signed query),
// not the stored CDN URL.
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, directPath string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetDirectPath(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetDirectPath(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetDirectPath(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetDirectPath(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		directPath,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database. direct_path is COALESCE'd to "" so legacy
// rows (written before the column existed, hence NULL) scan cleanly; the
// download path falls back to deriving it from the URL when empty.
func (store *MessageStore) GetMediaInfo(id, chatJID string) (mediaType, filename, url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64, err error) {
	err = store.db.QueryRow(
		"SELECT media_type, filename, url, COALESCE(direct_path, ''), media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &directPath, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url, directPath string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file. Media lives out-of-tree under
	// the same data dir as the databases (C4), one subdir per chat.
	dataDir, err := whatsappDataDir()
	if err != nil {
		return false, "", "", "", err
	}
	chatDir := filepath.Join(dataDir, strings.ReplaceAll(chatJID, ":", "_"))
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Create directory for the chat if it doesn't exist (owner-only, 0700)
	if err := os.MkdirAll(chatDir, 0700); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file. Sanitize the sender-controlled
	// filename with filepath.Base (C7) so a malicious "../.." name cannot
	// escape the chat's store dir.
	localPath = filepath.Join(chatDir, filepath.Base(filename))

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		// File exists, return it
		return true, mediaType, filename, absPath, nil
	}

	// If we don't have all the media info we need, we can't download. Either a
	// DirectPath or a URL (from which the path can be derived) is required,
	// along with the encryption metadata.
	if (directPath == "" && url == "") || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	fmt.Printf("Attempting to download media for message %s in chat %s...\n", messageID, chatJID)

	// Resolve the DirectPath whatsmeow needs. Prefer the proto-canonical
	// DirectPath we now persist; for legacy rows (stored before the column
	// existed) fall back to deriving it from the CDN URL. Either way the path
	// MUST retain its signed "?...&oh=&oe=&_nc_sid" query — whatsmeow builds the
	// authenticated request as host + directPath + "&hash=...", so a
	// query-stripped path produces an unauthenticated request and a 403.
	if directPath == "" {
		directPath = extractDirectPathFromURL(url)
	}
	if directPath == "" {
		return false, "", "", "", fmt.Errorf("no direct path available for download")
	}

	// Map our media type string to whatsmeow's MediaType.
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	// Download via whatsmeow's authenticated flow. DownloadMediaWithPath
	// refreshes the media connection (fresh CDN hosts) and re-derives the
	// request from the DirectPath + encryption metadata, then verifies the
	// SHA-256. This is the upstream-blessed replacement for the legacy
	// direct-URL download path removed in whatsmeow on 2026-05-31.
	mediaData, err := client.DownloadMediaWithPath(
		context.Background(),
		directPath,
		fileEncSHA256, // encFileHash
		fileSHA256,    // fileHash
		mediaKey,
		waMediaType,
		"",    // mmsType: derived from mediaType by whatsmeow
		false, // allowNoHash: hashes are present, enforce verification
	)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save the downloaded media to file
	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Successfully downloaded %s media to %s (%d bytes)\n", mediaType, absPath, len(mediaData))
	return true, mediaType, filename, absPath, nil
}

// extractDirectPathFromURL derives a whatsmeow DirectPath from a stored CDN
// URL. This is the LEGACY-ROW fallback only: new messages persist the proto
// DirectPath directly. whatsmeow builds the authenticated download as
// "https://<host>" + directPath + "&hash=...&mms-type=...", so the returned
// path MUST keep its signed query string (?ccb=...&oh=...&oe=...&_nc_sid=...).
// Stripping the query — as this function previously did — leaves the request
// unsigned and the CDN returns 403.
//
// Example URL:
//
//	https://mmg.whatsapp.net/v/t62.7117-24/<id>_n.enc?ccb=11-4&oh=<sig>&oe=<exp>&_nc_sid=<sid>&mms3=true
//
// yields:
//
//	/v/t62.7117-24/<id>_n.enc?ccb=11-4&oh=<sig>&oe=<exp>&_nc_sid=<sid>&mms3=true
func extractDirectPathFromURL(url string) string {
	// Split off the host to keep everything from the path onward, query included.
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return "" // Not a recognizable CDN URL; caller treats "" as unavailable.
	}
	return "/" + parts[1]
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, sendToken string, port int) {
	// Handler for sending text messages. LOCKED DOWN (HMB-333 Phase 2): the
	// listener binds loopback-only (see the bind at the bottom of this func) AND
	// every request must present the exact bearer token from the 0600
	// send-token file. Together those two properties mean ONLY the cockpit
	// send-gate — the sole holder of the token — can send: no other local
	// process, and nothing off-host, can reach or authenticate to this route.
	http.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Auth gate FIRST — before we read or parse the body. Missing token =>
		// 401; present-but-wrong => 403. The compare is constant-time so a
		// local attacker cannot recover the token via response timing.
		presented := presentedSendToken(r)
		if presented == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Missing send token", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(sendToken)) != 1 {
			http.Error(w, "Invalid send token", http.StatusForbidden)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.RecipientJID == "" {
			http.Error(w, "recipient_jid is required", http.StatusBadRequest)
			return
		}
		if req.Text == "" {
			http.Error(w, "text is required", http.StatusBadRequest)
			return
		}

		// Send the message
		success, messageID, message := sendTextMessage(r.Context(), client, req.RecipientJID, req.Text)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		json.NewEncoder(w).Encode(SendMessageResponse{
			Success:   success,
			Message:   message,
			MessageID: messageID,
		})
	})

	// Handler for downloading media
	http.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// Start the server. Bind to loopback ONLY (C2): the bridge's REST surface
	// must never be reachable from the LAN/VPN — only local processes on this
	// host may reach it. /api/download is loopback-gated; /api/send is
	// loopback-gated AND bearer-token-gated (cockpit send-gate only).
	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.ListenAndServe(serverAddr, nil); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Resolve the out-of-tree data directory (C4): both databases live under
	// ~/.config/homebase/whatsapp (0700), never inside the repo tree.
	dataDir, err := whatsappDataDir()
	if err != nil {
		logger.Errorf("%v", err)
		os.Exit(1)
	}
	sessionDBPath := filepath.Join(dataDir, "whatsapp.db")
	messagesDBPath := filepath.Join(dataDir, "messages.db")

	// Refuse to start if an existing DB is not owner-only (0600). The session DB
	// holds the linked-device credentials; a group/world-readable copy is a
	// custody failure, so we exit(1) before connecting.
	enforceMode600(sessionDBPath, "WhatsApp session database", logger)
	enforceMode600(messagesDBPath, "WhatsApp message database", logger)

	// Load (or first-run generate) the bearer token that gates /api/send. This
	// refuses to start on a mis-permissioned token, so custody is verified
	// before we ever connect — same posture as the DB checks above.
	sendToken := loadOrCreateSendToken(logger)

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:"+sessionDBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Lock the session DB down to owner-only (0600) after sqlstore created it.
	if err := os.Chmod(sessionDBPath, 0o600); err != nil {
		logger.Errorf("Failed to secure session database permissions: %v", err)
		os.Exit(1)
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore(messagesDBPath)
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				// Headless pairing: emit the raw QR payload on an opt-in env flag so
				// tooling can render a clean image (never printed in normal use).
				if os.Getenv("WA_HEADLESS_QR") == "1" {
					fmt.Println("WA_QR_RAW::" + evt.Code)
				}
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	startRESTServer(client, messageStore, sendToken, 8080)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url, directPath string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					directPath,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}
