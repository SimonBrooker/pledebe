package notify

import (
	"errors"
	"fmt"
	"net/smtp"
	"strings"
)

// autoAuth negotiates whichever authentication mechanism the server actually
// offers.
//
// Go's standard library ships PLAIN only. Microsoft 365 and Exchange advertise
// LOGIN and reject PLAIN with "504 5.7.4 Unrecognized authentication type",
// which reads like a credential problem but is a mechanism mismatch. Several
// other providers are the same way.
//
// Both mechanisms send the password in a trivially reversible encoding, so both
// are refused unless the connection is already encrypted.
type autoAuth struct {
	username string
	password string
	host     string

	chosen string
}

// newAuth returns nil when no username is configured, since many relays on a
// local network accept mail without authentication at all.
func newAuth(username, password, host string) smtp.Auth {
	if username == "" {
		return nil
	}
	return &autoAuth{username: username, password: password, host: host}
}

func (a *autoAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	// The same protection the standard library applies to PLAIN: never hand
	// over credentials in the clear. A relay on localhost is exempted because
	// there is no network to intercept.
	if !server.TLS && server.Name != "localhost" {
		return "", nil, errors.New(
			"refusing to send credentials over an unencrypted connection; " +
				"use port 587 (STARTTLS) or 465 (TLS)")
	}
	if server.Name != a.host && server.Name != "" {
		return "", nil, fmt.Errorf("unexpected server name %q, wanted %q", server.Name, a.host)
	}

	switch {
	case hasMechanism(server.Auth, "PLAIN"):
		a.chosen = "PLAIN"
		return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
	case hasMechanism(server.Auth, "LOGIN"):
		a.chosen = "LOGIN"
		return "LOGIN", nil, nil
	}

	return "", nil, fmt.Errorf(
		"server offers no authentication mechanism pledebe supports "+
			"(it advertises %s; pledebe speaks PLAIN and LOGIN). "+
			"Microsoft 365 accounts often need SMTP AUTH enabled, or an app password",
		strings.Join(server.Auth, ", "))
}

// Next answers the server's prompts during a LOGIN exchange.
//
// Prompt wording is not standardised — "Username:", "User Name:" and base64
// variations all occur — so it is matched loosely rather than compared exactly.
func (a *autoAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	if a.chosen != "LOGIN" {
		return nil, fmt.Errorf("unexpected challenge %q during %s", fromServer, a.chosen)
	}

	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.Contains(prompt, "user"):
		return []byte(a.username), nil
	case strings.Contains(prompt, "pass"):
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unrecognised LOGIN prompt %q", fromServer)
}

func hasMechanism(advertised []string, want string) bool {
	for _, m := range advertised {
		if strings.EqualFold(m, want) {
			return true
		}
	}
	return false
}
