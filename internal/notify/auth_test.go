package notify

import (
	"net/smtp"
	"strings"
	"testing"
)

// Microsoft 365 and Exchange advertise LOGIN and reject PLAIN with
// "504 5.7.4 Unrecognized authentication type" -- which reads like a credential
// problem but is a mechanism mismatch. Go's standard library speaks PLAIN only.
func TestAuthPrefersPlainButFallsBackToLogin(t *testing.T) {
	cases := []struct {
		name       string
		advertised []string
		want       string
	}{
		{"plain offered", []string{"PLAIN", "LOGIN"}, "PLAIN"},
		{"login only, as Microsoft 365 does", []string{"LOGIN"}, "LOGIN"},
		{"case insensitive", []string{"login"}, "LOGIN"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuth("user", "pass", "smtp.example")
			got, _, err := a.Start(&smtp.ServerInfo{
				Name: "smtp.example", TLS: true, Auth: tc.advertised,
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got != tc.want {
				t.Errorf("chose %q, want %q", got, tc.want)
			}
		})
	}
}

// The error has to say what to do. "Unrecognized authentication type" from the
// server tells the operator nothing about which knob to turn.
func TestAuthUnsupportedMechanismExplainsItself(t *testing.T) {
	a := newAuth("user", "pass", "smtp.example")
	_, _, err := a.Start(&smtp.ServerInfo{
		Name: "smtp.example", TLS: true, Auth: []string{"XOAUTH2", "NTLM"},
	})
	if err == nil {
		t.Fatal("expected an error for unsupported mechanisms")
	}
	for _, want := range []string{"XOAUTH2", "PLAIN and LOGIN", "app password"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %v", want, err)
		}
	}
}

// Both mechanisms send the password in a trivially reversible encoding.
func TestAuthRefusesUnencryptedConnection(t *testing.T) {
	a := newAuth("user", "pass", "smtp.example")
	_, _, err := a.Start(&smtp.ServerInfo{
		Name: "smtp.example", TLS: false, Auth: []string{"PLAIN"},
	})
	if err == nil {
		t.Fatal("sent credentials over an unencrypted connection")
	}
	if !strings.Contains(err.Error(), "587") {
		t.Errorf("error should suggest a working port; got %v", err)
	}
}

// A relay with no network between it and pledebe is exempt, matching the
// standard library's behaviour.
func TestAuthAllowsLocalhostWithoutTLS(t *testing.T) {
	a := newAuth("user", "pass", "localhost")
	if _, _, err := a.Start(&smtp.ServerInfo{
		Name: "localhost", TLS: false, Auth: []string{"PLAIN"},
	}); err != nil {
		t.Errorf("localhost relay refused: %v", err)
	}
}

// LOGIN prompts are not standardised: "Username:", "User Name:" and other
// wordings all occur, so they are matched loosely.
func TestLoginChallengeResponses(t *testing.T) {
	a := newAuth("someone@example", "secret", "smtp.example")
	if _, _, err := a.Start(&smtp.ServerInfo{
		Name: "smtp.example", TLS: true, Auth: []string{"LOGIN"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, prompt := range []string{"Username:", "User Name:", "username"} {
		got, err := a.Next([]byte(prompt), true)
		if err != nil {
			t.Fatalf("%q: %v", prompt, err)
		}
		if string(got) != "someone@example" {
			t.Errorf("%q answered %q, want the username", prompt, got)
		}
	}
	for _, prompt := range []string{"Password:", "password"} {
		got, err := a.Next([]byte(prompt), true)
		if err != nil {
			t.Fatalf("%q: %v", prompt, err)
		}
		if string(got) != "secret" {
			t.Errorf("%q answered %q, want the password", prompt, got)
		}
	}

	// Nothing more to send once the server stops prompting.
	if got, err := a.Next(nil, false); err != nil || got != nil {
		t.Errorf("final step returned %q, %v; want nil, nil", got, err)
	}
}

// No username means an unauthenticated relay, which is common on a LAN.
func TestNoUsernameMeansNoAuth(t *testing.T) {
	if a := newAuth("", "", "smtp.example"); a != nil {
		t.Error("expected nil auth when no username is configured")
	}
}
