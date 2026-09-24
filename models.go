package main

import "strings"

const (
	providerID         = "excel"
	defaultPublicModel = "gpt-5.6-sol-excel"
	defaultUpstream    = "gpt-5.6-sol"
)

type excelModel struct {
	PublicID   string
	UpstreamID string
	Display    string
	Context    int64
}

var excelModels = []excelModel{
	{PublicID: "gpt-6-astra-excel", UpstreamID: "gpt-6-astra", Display: "GPT 6 Astra Excel", Context: 272000},
	{PublicID: "gpt-5.6-sol-excel", UpstreamID: "gpt-5.6-sol", Display: "5.6 Sol Excel", Context: 272000},
	{PublicID: "gpt-5.6-terra-excel", UpstreamID: "gpt-5.6-terra", Display: "5.6 Terra Excel", Context: 272000},
	{PublicID: "gpt-5.6-luna-excel", UpstreamID: "gpt-5.6-luna", Display: "5.6 Luna Excel", Context: 200000},
}

var reasoningEfforts = []string{"low", "medium", "high", "xhigh"}

var reasoningAliases = map[string]string{
	"x-high":     "xhigh",
	"extra-high": "xhigh",
	"extra_high": "xhigh",
	"max":        "xhigh",
	"ultra":      "xhigh",
}

func knownModel(publicID string) (excelModel, bool) {
	for _, model := range excelModels {
		if model.PublicID == publicID || model.UpstreamID == publicID {
			return model, true
		}
	}
	return excelModel{}, false
}

func splitModelSuffix(model string) (string, string) {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, providerID+"/")
	open := strings.LastIndex(model, "(")
	if open == -1 || !strings.HasSuffix(model, ")") || open == len(model)-1 {
		return model, ""
	}
	return model[:open], strings.TrimSpace(model[open+1 : len(model)-1])
}

func publicModelID(model string) string {
	name, _ := splitModelSuffix(model)
	if name == "" {
		return defaultPublicModel
	}
	if known, ok := knownModel(name); ok && !strings.HasSuffix(name, "-excel") && name == known.UpstreamID {
		return known.PublicID
	}
	return name
}

func upstreamModelID(model string) string {
	name, _ := splitModelSuffix(model)
	if name == "" {
		return defaultUpstream
	}
	if strings.HasSuffix(name, "-excel") {
		if known, ok := knownModel(name); ok {
			return known.UpstreamID
		}
		trimmed := strings.TrimSuffix(name, "-excel")
		if trimmed != "" {
			return trimmed
		}
	}
	if known, ok := knownModel(name); ok {
		return known.UpstreamID
	}
	return name
}

func normalizeEffort(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	normalized := strings.ToLower(strings.TrimSpace(text))
	if alias, ok := reasoningAliases[normalized]; ok {
		normalized = alias
	}
	for _, effort := range reasoningEfforts {
		if normalized == effort {
			return effort
		}
	}
	return ""
}

func effortFromModelSuffix(model string) string {
	_, suffix := splitModelSuffix(model)
	if suffix == "" {
		return ""
	}
	if _, err := atoiOK(suffix); err {
		return ""
	}
	return normalizeEffort(suffix)
}

func atoiOK(text string) (int, bool) {
	parsed := atoiDefault(text, -1)
	if parsed < 0 || (parsed == -1 && strings.TrimSpace(text) != "-1") {
		return 0, false
	}
	return parsed, true
}
