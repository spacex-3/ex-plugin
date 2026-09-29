package main

import (
	"encoding/base64"
	"strings"
	"sync"
)

const (
	maxGeneratedImages     = 8
	maxGeneratedImageBytes = 8 << 20
)

type cachedGeneratedImage struct {
	id      string
	dataURL string
	final   bool
}

var generatedImageCache struct {
	mu    sync.Mutex
	order []string
	items map[string]cachedGeneratedImage
}

func resetGeneratedImages() {
	generatedImageCache.mu.Lock()
	generatedImageCache.order = nil
	generatedImageCache.items = nil
	generatedImageCache.mu.Unlock()
}

func rememberGeneratedImages(value any) {
	rememberGeneratedImagesDepth(value, 0)
}

func rememberGeneratedImagesDepth(value any, depth int) {
	if value == nil || depth > 8 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		rememberGeneratedImageObject(typed)
		for _, child := range typed {
			rememberGeneratedImagesDepth(child, depth+1)
		}
	case []any:
		for _, child := range typed {
			rememberGeneratedImagesDepth(child, depth+1)
		}
	}
}

func rememberGeneratedImageObject(object map[string]any) {
	id := generatedImageID(object)
	if id == "" {
		return
	}
	dataURL, final, ok := generatedImageDataURL(object)
	if !ok {
		return
	}
	storeGeneratedImage(id, dataURL, final)
}

func generatedImageID(object map[string]any) string {
	for _, key := range []string{"id", "item_id"} {
		id := strings.TrimSpace(stringField(object, key))
		if strings.HasPrefix(strings.ToLower(id), "ig_") {
			return id
		}
	}
	return ""
}

func generatedImageDataURL(object map[string]any) (string, bool, bool) {
	for _, key := range []string{"result", "b64_json", "image_base64"} {
		if dataURL, ok := imageDataURL(stringField(object, key)); ok {
			return dataURL, true, true
		}
	}
	if dataURL, ok := imageDataURL(imageURLString(object)); ok {
		return dataURL, true, true
	}
	if dataURL, ok := imageDataURL(stringField(object, "url")); ok {
		return dataURL, true, true
	}
	if dataURL, ok := imageDataURL(stringField(object, "partial_image_b64")); ok {
		return dataURL, false, true
	}
	return "", false, false
}

func imageDataURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxGeneratedImageBytes*2 {
		return "", false
	}
	if isDataImageURL(raw) {
		if _, err := decodeInlineImage(raw); err != nil {
			return "", false
		}
		return raw, true
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 || len(decoded) > maxGeneratedImageBytes {
		return "", false
	}
	mediaType, _, ok := sniffImage(decoded)
	if !ok {
		return "", false
	}
	return "data:" + mediaType + ";base64," + raw, true
}

func storeGeneratedImage(id, dataURL string, final bool) {
	generatedImageCache.mu.Lock()
	defer generatedImageCache.mu.Unlock()
	if generatedImageCache.items == nil {
		generatedImageCache.items = map[string]cachedGeneratedImage{}
	}
	if existing, ok := generatedImageCache.items[id]; ok && existing.final && !final {
		return
	}
	next := make([]string, 0, len(generatedImageCache.order)+1)
	for _, existing := range generatedImageCache.order {
		if existing != id {
			next = append(next, existing)
		}
	}
	next = append(next, id)
	generatedImageCache.order = next
	generatedImageCache.items[id] = cachedGeneratedImage{id: id, dataURL: dataURL, final: final}
	for len(generatedImageCache.order) > maxGeneratedImages {
		drop := generatedImageCache.order[0]
		generatedImageCache.order = generatedImageCache.order[1:]
		delete(generatedImageCache.items, drop)
	}
}

func lookupGeneratedImage(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	generatedImageCache.mu.Lock()
	defer generatedImageCache.mu.Unlock()
	item, ok := generatedImageCache.items[id]
	if !ok {
		return ""
	}
	return item.dataURL
}

func replayGeneratedImageItem(item map[string]any) ([]any, bool) {
	itemType := strings.ToLower(strings.TrimSpace(stringField(item, "type")))
	id := generatedImageID(item)
	switch itemType {
	case "image_generation_call", "image_generation_call_output":
	case "item_reference":
		if id == "" {
			return nil, false
		}
	default:
		if id == "" || itemType == "message" || itemType == "function_call" || itemType == "custom_tool_call" || itemType == "reasoning" {
			return nil, false
		}
	}
	rememberGeneratedImageObject(item)
	dataURL := lookupGeneratedImage(id)
	if dataURL == "" {
		if inline, _, ok := generatedImageDataURL(item); ok {
			dataURL = inline
		}
	}
	if dataURL == "" {
		return nil, true
	}
	return []any{userImageMessage(dataURL)}, true
}

func restoreGeneratedImageParts(item map[string]any) []any {
	content, ok := item["content"].([]any)
	if !ok {
		return []any{item}
	}
	kept := make([]any, 0, len(content))
	var images []any
	changed := false
	for _, part := range content {
		object, ok := part.(map[string]any)
		if !ok {
			kept = append(kept, part)
			continue
		}
		replacement, matched := takeGeneratedContent(object)
		if !matched {
			kept = append(kept, part)
			continue
		}
		changed = true
		if replacement != nil {
			images = append(images, replacement)
		}
	}
	if !changed {
		return []any{item}
	}
	var out []any
	if len(kept) > 0 {
		cloned := cloneObject(item)
		cloned["content"] = kept
		out = append(out, cloned)
	}
	if len(images) > 0 {
		out = append(out, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": images,
		})
	}
	return out
}

func takeGeneratedContent(part map[string]any) (map[string]any, bool) {
	id := generatedImageID(part)
	kind := strings.ToLower(strings.TrimSpace(stringField(part, "type")))
	if id == "" && kind != "image_generation" && kind != "image_generation_call" && kind != "image_generation_call_output" {
		return nil, false
	}
	rememberGeneratedImageObject(part)
	dataURL := lookupGeneratedImage(id)
	if dataURL == "" {
		if inline, _, ok := generatedImageDataURL(part); ok {
			dataURL = inline
		}
	}
	if dataURL == "" {
		return nil, true
	}
	return inputImagePart(dataURL), true
}

func userImageMessage(dataURL string) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{inputImagePart(dataURL)},
	}
}

func inputImagePart(dataURL string) map[string]any {
	return map[string]any{
		"type":      "input_image",
		"image_url": dataURL,
		"detail":    "auto",
	}
}
