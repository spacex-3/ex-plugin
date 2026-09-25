package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDataImageBecomesFileID(t *testing.T) {
	resetAttachmentCache()
	restore := stubAttachmentUpload(t, "file_png")
	defer restore()
	body := imageBody("user", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("png-bytes")))
	if err := uploadInputImages(imageCred("token-a"), defaultResponses, body); err != nil {
		t.Fatal(err)
	}
	part := imagePart(t, body)
	if part["file_id"] != "file_png" || part["image_url"] != nil || part["detail"] != "auto" {
		t.Fatalf("part = %#v", part)
	}
}

func TestImageUploadIsCachedForTheSameCredential(t *testing.T) {
	resetAttachmentCache()
	var calls int
	restore := stubAttachmentUploadCount(t, &calls, "file_cached")
	defer restore()
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("same-bytes"))
	for range 2 {
		body := imageBody("user", url)
		if err := uploadInputImages(imageCred("token-a"), defaultResponses, body); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("uploads = %d", calls)
	}
}

func TestImageAndFileIDTogetherAreRejected(t *testing.T) {
	resetAttachmentCache()
	body := imageBody("user", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("png-bytes")))
	part := imagePart(t, body)
	part["file_id"] = "file_existing"
	err := uploadInputImages(imageCred("token-a"), defaultResponses, body)
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("err = %v", err)
	}
}

func TestAssistantImageIsNotUploaded(t *testing.T) {
	resetAttachmentCache()
	var calls int
	restore := stubAttachmentUploadCount(t, &calls, "file_nope")
	defer restore()
	body := imageBody("assistant", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("png-bytes")))
	if err := uploadInputImages(imageCred("token-a"), defaultResponses, body); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || imagePart(t, body)["image_url"] == nil {
		t.Fatalf("assistant image changed, calls=%d", calls)
	}
}

func TestChatImageURLIsUploaded(t *testing.T) {
	resetAttachmentCache()
	restore := stubAttachmentUpload(t, "file_chat")
	defer restore()
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("jpeg-bytes"))
	source := chatToResponses(map[string]any{
		"model": "gpt-5.6-sol",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}, "detail": "high"},
			},
		}},
	})
	body := prepareResponsesBody(source, "")
	if err := uploadInputImages(imageCred("token-a"), defaultResponses, body); err != nil {
		t.Fatal(err)
	}
	parts := userContent(t, body)
	found := false
	for _, value := range parts {
		part, _ := value.(map[string]any)
		if part["type"] == "input_image" {
			found = true
			if part["file_id"] != "file_chat" || part["detail"] != "high" || part["image_url"] != nil {
				t.Fatalf("part = %#v", part)
			}
		}
	}
	if !found {
		t.Fatalf("content = %#v", parts)
	}
}

func TestAttachmentURLStaysBesideResponses(t *testing.T) {
	endpoint, err := attachmentURL(defaultResponses)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://bps.openai.com/basispoints/api/attachments" {
		t.Fatalf("endpoint = %s", endpoint)
	}
}

func imageCred(token string) credential {
	return credential{
		AccessToken: token,
		AccountID:   "acct",
		AuthMode:    "chatgpt",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

func imageBody(role, imageURL string) map[string]any {
	return map[string]any{
		"input": []any{map[string]any{
			"type": "message",
			"role": role,
			"content": []any{
				map[string]any{"type": "input_text", "text": "look"},
				map[string]any{"type": "input_image", "image_url": imageURL},
			},
		}},
	}
}

func imagePart(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	for _, value := range userContent(t, body) {
		part, _ := value.(map[string]any)
		if part["type"] == "input_image" {
			return part
		}
	}
	t.Fatal("missing input_image")
	return nil
}

func userContent(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, _ := body["input"].([]any)
	for _, value := range items {
		item, _ := value.(map[string]any)
		if stringField(item, "role") == "user" || stringField(item, "role") == "assistant" {
			if content, ok := item["content"].([]any); ok {
				return content
			}
		}
	}
	t.Fatalf("missing content in %#v", body["input"])
	return nil
}

func stubAttachmentUpload(t *testing.T, fileID string) func() {
	t.Helper()
	var calls int
	return stubAttachmentUploadCount(t, &calls, fileID)
}

func stubAttachmentUploadCount(t *testing.T, calls *int, fileID string) func() {
	t.Helper()
	previous := doHost
	doHost = func(method string, payload any) (json.RawMessage, error) {
		if method != hostHTTPDo {
			t.Errorf("method = %s", method)
		}
		request, _ := payload.(map[string]any)
		if request["url"] != "https://bps.openai.com/basispoints/api/attachments" {
			t.Errorf("url = %#v", request["url"])
		}
		*calls++
		raw, err := json.Marshal(map[string]any{
			"status_code": 200,
			"body":        []byte(`{"openai_file_id":"` + fileID + `"}`),
		})
		return raw, err
	}
	return func() { doHost = previous }
}
