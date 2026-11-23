package config

import (
	"kichat/internal/llm"
	"strings"
)

// GatewaySettings exposes one explicitly selected model to external clients. The
// credential lives in the secret store, never in this settings blob.
type GatewaySettings struct {
	Enabled bool    `json:"enabled"`
	Port    int     `json:"port"`
	Model   llm.Ref `json:"model"`
	// ClientModel is the public alias; Model still selects the actual provider/model.
	ClientModel   string `json:"clientModel"`
	ContextLimit  int    `json:"contextLimit"`
	ReserveTokens int    `json:"reserveTokens"`
	Compact       bool   `json:"compact"`
}

const GatewayKeyName = "external-api"

func DefaultGateway() GatewaySettings {
	return GatewaySettings{Port: 17841, ClientModel: "kichat", ContextLimit: 16384, ReserveTokens: 2048, Compact: true}
}

func NormaliseGateway(s GatewaySettings) GatewaySettings {
	s.ClientModel = strings.TrimSpace(s.ClientModel)
	if s.ClientModel == "" {
		s.ClientModel = DefaultGateway().ClientModel
	}
	if s.Port < 1024 || s.Port > 65535 {
		s.Port = 17841
	}
	if s.ContextLimit < 2048 || s.ContextLimit > 1048576 {
		s.ContextLimit = 16384
	}
	if s.ReserveTokens < 128 || s.ReserveTokens > s.ContextLimit/2 {
		s.ReserveTokens = min(2048, s.ContextLimit/4)
	}
	return s
}
