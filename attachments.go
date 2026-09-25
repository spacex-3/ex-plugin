package main

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
)

const maxAttachmentCacheEntries = 512

type attachmentError struct {
	status  int
	code    string
	message string
}

func (e *attachmentError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *attachmentError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *attachmentError) Code() string {
	if e == nil || e.code == "" {
		return "attachment_upload_error"
	}
	return e.code
}

func attachmentFail(status int, code, message string) error {
	return &attachmentError{status: status, code: code, message: message}
}

type cachedAttachment struct {
	key    [sha256.Size]byte
	fileID string
}

type pendingAttachment struct {
	done   chan struct{}
	fileID string
	err    error
}

type attachmentCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*list.Element
	order   list.List
	pending map[[sha256.Size]byte]*pendingAttachment
}

var attachmentUploads attachmentCache

func resetAttachmentCache() {
	attachmentUploads.mu.Lock()
	defer attachmentUploads.mu.Unlock()
	attachmentUploads.entries = nil
	attachmentUploads.order.Init()
	attachmentUploads.pending = nil
}

func (c *attachmentCache) getOrUpload(key [sha256.Size]byte, upload func() (string, error)) (string, error) {
	c.mu.Lock()
	if c.entries != nil {
		if entry := c.entries[key]; entry != nil {
			c.order.MoveToFront(entry)
			fileID := entry.Value.(cachedAttachment).fileID
			c.mu.Unlock()
			return fileID, nil
		}
	}
	if pending := c.pending[key]; pending != nil {
		c.mu.Unlock()
		<-pending.done
		return pending.fileID, pending.err
	}
	if c.pending == nil {
		c.pending = make(map[[sha256.Size]byte]*pendingAttachment)
	}
	pending := &pendingAttachment{done: make(chan struct{})}
	c.pending[key] = pending
	c.mu.Unlock()

	fileID, err := upload()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
	if err == nil {
		if c.entries == nil {
			c.entries = make(map[[sha256.Size]byte]*list.Element)
		}
		c.entries[key] = c.order.PushFront(cachedAttachment{key: key, fileID: fileID})
		for c.order.Len() > maxAttachmentCacheEntries {
			oldest := c.order.Back()
			delete(c.entries, oldest.Value.(cachedAttachment).key)
			c.order.Remove(oldest)
		}
	}
	pending.fileID, pending.err = fileID, err
	close(pending.done)
	return fileID, err
}

type inlineImage struct {
	mediaType string
	ext       string
	data      []byte
}

func uploadInputImages(cred credential, responsesURL string, body map[string]any) error {
	items, _ := body["input"].([]any)
	for index, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringField(item, "role")))
		itemType := strings.ToLower(strings.TrimSpace(stringField(item, "type")))
		if role != "user" || (itemType != "" && itemType != "message") {
			continue
		}
		parts, _ := item["content"].([]any)
		var updated []any
		for partIndex, partValue := range parts {
			part, ok := partValue.(map[string]any)
			if !ok || strings.ToLower(stringField(part, "type")) != "input_image" {
				continue
			}
			imageURL := imageURLString(part)
			fileID := strings.TrimSpace(stringField(part, "file_id"))
			if fileID != "" && isDataImageURL(imageURL) {
				return attachmentFail(http.StatusBadRequest, "invalid_image", "input_image cannot contain both image_url and file_id")
			}
			if !isDataImageURL(imageURL) {
				continue
			}
			image, err := decodeInlineImage(imageURL)
			if err != nil {
				return err
			}
			endpoint, err := attachmentURL(responsesURL)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, _ = hash.Write([]byte(endpoint))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write([]byte(cred.AccountID))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write([]byte(cred.AuthMode))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write([]byte(cred.AccessToken))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write([]byte(image.mediaType))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write(image.data)
			var key [sha256.Size]byte
			copy(key[:], hash.Sum(nil))
			uploadedID, err := attachmentUploads.getOrUpload(key, func() (string, error) {
				return uploadImage(endpoint, image, cred)
			})
			if err != nil {
				return err
			}
			if updated == nil {
				updated = append([]any{}, parts...)
			}
			copyPart := cloneObject(part)
			delete(copyPart, "image_url")
			copyPart["file_id"] = uploadedID
			if strings.TrimSpace(stringField(copyPart, "detail")) == "" {
				copyPart["detail"] = "auto"
			}
			updated[partIndex] = copyPart
		}
		if updated != nil {
			copyItem := cloneObject(item)
			copyItem["content"] = updated
			items[index] = copyItem
		}
	}
	return nil
}

func isDataImageURL(value string) bool {
	return len(value) >= 5 && strings.EqualFold(value[:5], "data:")
}

func imageURLString(part map[string]any) string {
	switch typed := part["image_url"].(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return strings.TrimSpace(stringField(typed, "url"))
	default:
		return ""
	}
}

func decodeInlineImage(dataURL string) (inlineImage, error) {
	metadata, encoded, found := strings.Cut(dataURL[5:], ",")
	if !found {
		return inlineImage{}, attachmentFail(http.StatusBadRequest, "invalid_image", "input_image data URL is missing its data separator")
	}
	isBase64 := strings.HasSuffix(strings.ToLower(metadata), ";base64")
	if isBase64 {
		metadata = metadata[:len(metadata)-len(";base64")]
	}
	mediaType, _, err := mime.ParseMediaType(metadata)
	if err != nil || !strings.HasPrefix(mediaType, "image/") {
		return inlineImage{}, attachmentFail(http.StatusBadRequest, "invalid_image", "input_image data URL must declare an image media type")
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return inlineImage{}, attachmentFail(http.StatusBadRequest, "invalid_image", "input_image data URL has invalid percent encoding")
	}
	var data []byte
	if isBase64 {
		data, err = base64.StdEncoding.DecodeString(decoded)
	} else {
		data = []byte(decoded)
	}
	if err != nil || len(data) == 0 {
		return inlineImage{}, attachmentFail(http.StatusBadRequest, "invalid_image", "input_image data URL contains empty or invalid image data")
	}
	normalizedType, ext, ok := imageFileType(mediaType, data)
	if !ok {
		return inlineImage{}, attachmentFail(http.StatusBadRequest, "invalid_image", "input_image must be jpeg, png, gif, or webp")
	}
	return inlineImage{mediaType: normalizedType, ext: ext, data: data}, nil
}

func attachmentURL(responsesURL string) (string, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(responsesURL), "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", attachmentFail(http.StatusInternalServerError, "invalid_config", "cannot derive attachments endpoint from responses_url")
	}
	return base.ResolveReference(&url.URL{Path: "attachments"}).String(), nil
}

func uploadImage(endpoint string, image inlineImage, cred credential) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filename := "image" + image.ext
	partHeaders := make(textproto.MIMEHeader)
	partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name":     "file",
		"filename": filename,
	}))
	partHeaders.Set("Content-Type", image.mediaType)
	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		return "", attachmentFail(http.StatusInternalServerError, "attachment_encoding", "cannot encode image attachment")
	}
	if _, err := part.Write(image.data); err != nil {
		return "", attachmentFail(http.StatusInternalServerError, "attachment_encoding", "cannot write image attachment")
	}
	if err := writer.Close(); err != nil {
		return "", attachmentFail(http.StatusInternalServerError, "attachment_encoding", "cannot finish image attachment")
	}
	headers, _, err := cred.requestHeaders(false)
	if err != nil {
		return "", attachmentFail(http.StatusUnauthorized, "invalid_api_key", err.Error())
	}
	headers["content-type"] = []string{writer.FormDataContentType()}
	headers["accept"] = []string{"application/json"}
	raw, err := doHost(hostHTTPDo, map[string]any{
		"method":  http.MethodPost,
		"url":     endpoint,
		"headers": headers,
		"body":    body.Bytes(),
	})
	if err != nil {
		return "", attachmentFail(http.StatusBadGateway, "attachment_transport", "Basis Points attachment upload failed: "+redactAttachmentError(err.Error(), cred, image))
	}
	var response struct {
		StatusCode int    `json:"status_code"`
		Body       []byte `json:"body"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", attachmentFail(http.StatusBadGateway, "attachment_transport", "Basis Points attachment upload returned invalid host response")
	}
	if response.StatusCode == 0 {
		response.StatusCode = http.StatusOK
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", attachmentFail(response.StatusCode, "attachment_upload_error", fmt.Sprintf("Basis Points attachment upload HTTP %d: %s", response.StatusCode, redactAttachmentError(upstreamErrorMessage(response.Body), cred, image)))
	}
	var result struct {
		FileID string `json:"openai_file_id"`
	}
	if json.Unmarshal(response.Body, &result) != nil || strings.TrimSpace(result.FileID) == "" {
		return "", attachmentFail(http.StatusBadGateway, "invalid_attachment_response", "Basis Points attachment upload returned no openai_file_id")
	}
	return strings.TrimSpace(result.FileID), nil
}

func redactAttachmentError(message string, cred credential, image inlineImage) string {
	for _, secret := range []string{cred.AccessToken, cred.AccountID, cred.Email, base64.StdEncoding.EncodeToString(image.data)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return truncate(message, 500)
}

func imageFileType(declared string, data []byte) (string, string, bool) {
	if mediaType, ext, ok := sniffImage(data); ok {
		return mediaType, ext, true
	}
	switch strings.ToLower(strings.TrimSpace(declared)) {
	case "image/jpeg", "image/jpg", "image/pjpeg", "image/jfif", "image/jpe":
		return "image/jpeg", ".jpg", true
	case "image/png", "image/x-png":
		return "image/png", ".png", true
	case "image/gif":
		return "image/gif", ".gif", true
	case "image/webp":
		return "image/webp", ".webp", true
	default:
		return "", "", false
	}
}

func sniffImage(data []byte) (string, string, bool) {
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg", ".jpg", true
	}
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		return "image/png", ".png", true
	}
	if len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))) {
		return "image/gif", ".gif", true
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return "image/webp", ".webp", true
	}
	return "", "", false
}
