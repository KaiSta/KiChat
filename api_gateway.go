package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"kichat/internal/config"
	"kichat/internal/gateway"
	"kichat/internal/llm"
	"kichat/internal/uimsg"
)

func validateGateway(s config.Settings) error {
	g := s.Gateway
	if strings.TrimSpace(g.Model.Model) == "" {
		return uimsg.Errorf("gateway.chooseModel")
	}
	for _, b := range s.Backends {
		if b.ID != g.Model.Backend || !b.Enabled {
			continue
		}
		u, err := url.Parse(b.BaseURL)
		if err != nil {
			return uimsg.Errorf("gateway.chooseModel")
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		if (strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())) && u.Port() == strconv.Itoa(g.Port) {
			return uimsg.Errorf("gateway.loop")
		}
		return nil
	}
	return uimsg.Errorf("gateway.chooseModel")
}

// gatewayKey serialises first-time key creation across settings and reveal calls.
func (a *App) gatewayKey() (string, error) {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	return a.gatewayKeyLocked()
}

func (a *App) gatewayKeyLocked() (string, error) {
	if key, ok := a.secrets.Get(config.GatewayKeyName); ok && key != "" {
		return key, nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	key := hex.EncodeToString(b[:])
	if err := a.secrets.Set(config.GatewayKeyName, key); err != nil {
		return "", err
	}
	return key, nil
}

// GetGatewayKey is an explicit reveal for copying into an external application.
// Neither settings nor the periodically polled status includes the credential.
func (a *App) GetGatewayKey() (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	return a.gatewayKey()
}

func (a *App) GetGatewayStatus() (gateway.Status, error) {
	if err := a.ensure(); err != nil {
		return gateway.Status{}, err
	}
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	address := fmt.Sprintf("http://127.0.0.1:%d/v1", a.Settings().Gateway.Port)
	status := gateway.Status{URL: address}
	if a.gateway != nil {
		status = a.gateway.Status()
	}
	if !status.Running {
		status.URL = address
	}
	if a.gatewayError != "" {
		status.Error = a.gatewayError
	}
	return status, nil
}

func (a *App) applyGateway() {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	s := a.Settings()
	g := s.Gateway
	if a.gateway != nil && a.gatewayConfig == g && a.gateway.Status().Running {
		return
	}
	if a.gateway != nil {
		if err := a.gateway.Stop(); err != nil {
			a.gatewayError = err.Error()
			return
		}
	}
	a.gatewayConfig, a.gatewayError = g, ""
	if !g.Enabled {
		return
	}
	if err := validateGateway(s); err != nil {
		a.gatewayError = err.Error()
		return
	}
	if _, err := a.gatewayKeyLocked(); err != nil {
		a.gatewayError = err.Error()
		return
	}
	if a.gateway == nil {
		a.gateway = gateway.New(a.resolveGateway)
	}
	if err := a.gateway.Start(g.Port); err != nil {
		a.gatewayError = err.Error()
	}
}

func (a *App) stopGateway() {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	if a.gateway != nil {
		if err := a.gateway.Stop(); err != nil {
			a.gatewayError = err.Error()
		}
	}
}

func (a *App) resolveGateway(_ context.Context) (llm.Provider, gateway.Options, error) {
	s := a.Settings()
	g := s.Gateway
	if !g.Enabled {
		return nil, gateway.Options{}, uimsg.Errorf("gateway.disabled")
	}
	if err := validateGateway(s); err != nil {
		return nil, gateway.Options{}, err
	}
	p, ok := a.currentRegistry().Provider(g.Model.Backend)
	if !ok {
		return nil, gateway.Options{}, uimsg.Errorf("gateway.chooseModel")
	}
	key, ok := a.secrets.Get(config.GatewayKeyName)
	if !ok {
		return nil, gateway.Options{}, uimsg.Errorf("gateway.keyMissing")
	}
	limit := g.ContextLimit
	for _, model := range a.currentRegistry().Snapshot() {
		if model.Backend == g.Model.Backend && model.ID == g.Model.Model && model.ContextLimit > 0 {
			limit = min(limit, model.ContextLimit)
		}
	}
	if over := s.ModelOverrides[g.Model.String()]; over.ContextLimit > 0 {
		limit = min(limit, over.ContextLimit)
	}
	if over := s.ModelOverrides[g.Model.String()]; over.RequestCeiling > 0 {
		limit = min(limit, over.RequestCeiling)
	}
	for _, b := range s.Backends {
		if b.ID == g.Model.Backend && b.Kind == "llamacpp" {
			if loaded := s.Runtime.Models[b.ID].CtxSize; loaded > 0 {
				limit = min(limit, loaded)
			}
		}
	}
	// Changes to the upstream address, model or configuration invalidate summary
	// reuse even when the client repeats an identical conversation prefix.
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, gateway.Options{}, err
	}
	hash := sha256.Sum256(raw)
	return p, gateway.Options{Key: key, Model: g.Model.Model, ClientModel: g.ClientModel, Scope: hex.EncodeToString(hash[:]),
		ContextLimit: limit, ReserveTokens: g.ReserveTokens, Compact: g.Compact}, nil
}
