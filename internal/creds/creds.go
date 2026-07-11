// Package creds resolves and stores API credentials for paid model
// providers. The rules, in order, and the guarantees:
//
//  1. Environment variable (ANTHROPIC_API_KEY / OPENAI_API_KEY) — used if
//     set, never stored anywhere by mason.
//  2. OS keychain (macOS Keychain, Windows Credential Manager, Linux
//     Secret Service) under service "mason" — the ONLY place mason ever
//     persists a credential.
//  3. Interactive prompt (only on a TTY), echo off, with an offer to save
//     to the keychain.
//
// A key is never written to config files, session transcripts, logs, or
// the shale trail. Provider error paths additionally scrub the key value
// (see internal/provider).
package creds

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/term"
)

const service = "mason"

var envVar = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
	"gemini":    "GEMINI_API_KEY",
}

// Has reports whether a credential is available for vendor without
// prompting (env or keychain). Safe to call during model auto-detection.
func Has(vendor string) bool {
	if ev, ok := envVar[vendor]; ok && os.Getenv(ev) != "" {
		return true
	}
	k, err := keyring.Get(service, vendor)
	return err == nil && k != ""
}

// Get resolves the credential for vendor. If interactive is true and no
// stored credential exists, it prompts on the terminal (echo off) and
// offers to store the key in the OS keychain.
func Get(vendor string, interactive bool) (string, error) {
	ev, ok := envVar[vendor]
	if !ok {
		return "", fmt.Errorf("unknown provider %q", vendor)
	}
	if k := os.Getenv(ev); k != "" {
		return k, nil
	}
	if k, err := keyring.Get(service, vendor); err == nil && k != "" {
		return k, nil
	}
	if !interactive || !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("no credential for %s: set %s or run `mason login %s`", vendor, ev, vendor)
	}
	return promptAndOfferStore(vendor)
}

// Store saves the credential to the OS keychain.
func Store(vendor, key string) error {
	if _, ok := envVar[vendor]; !ok {
		return fmt.Errorf("unknown provider %q", vendor)
	}
	return keyring.Set(service, vendor, key)
}

// Delete removes the stored credential for vendor from the OS keychain.
func Delete(vendor string) error {
	if _, ok := envVar[vendor]; !ok {
		return fmt.Errorf("unknown provider %q", vendor)
	}
	return keyring.Delete(service, vendor)
}

// Login prompts for a key (echo off) and stores it in the OS keychain.
// Used by `mason login <provider>`.
func Login(vendor string) error {
	if _, ok := envVar[vendor]; !ok {
		return fmt.Errorf("unknown provider %q (anthropic | openai | gemini)", vendor)
	}
	key, err := readSecret(fmt.Sprintf("%s API key: ", vendor))
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("empty key, nothing stored")
	}
	if err := Store(vendor, key); err != nil {
		return fmt.Errorf("keychain store failed: %w", err)
	}
	fmt.Printf("stored %s credential in the OS keychain (service %q)\n", vendor, service)
	return nil
}

func promptAndOfferStore(vendor string) (string, error) {
	key, err := readSecret(fmt.Sprintf("%s API key (input hidden): ", vendor))
	if err != nil || key == "" {
		return "", fmt.Errorf("no credential for %s", vendor)
	}
	fmt.Print("save to OS keychain for next time? [y/N] ")
	r := bufio.NewReader(os.Stdin)
	ans, _ := r.ReadString('\n')
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
		if err := Store(vendor, key); err != nil {
			fmt.Fprintf(os.Stderr, "keychain store failed (key used for this session only): %v\n", err)
		} else {
			fmt.Println("stored in OS keychain")
		}
	}
	return key, nil
}

// readSecret reads one line from the terminal with echo disabled, so the
// key is never visible on screen or in terminal scrollback.
func readSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
