// browser_security.go — Four-layer security perimeter for browser automation.
//
// Same deny-list → warn-list → approval-gate → audit-log pattern as the
// Swift FunctionBridge and Guardian Shield. Protects against prompt injection
// attacks that try to exfiltrate cookies/tokens from authenticated sessions.
//
// Layer 1: Pattern block — hard-deny dangerous JS before it reaches the browser
// Layer 2: Domain protection — flag sensitive domains (AWS, GitHub, banking, etc.)
// Layer 3: TTY confirmation — require human approval on sensitive domains
// Layer 4: Audit log — JSONL trail of every command, allowed or denied

package browser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/strutil"

	"gopkg.in/yaml.v3"
)

// ─── types ────────────────────────────────────────────────────────────────

// browserPolicy is loaded from ~/.qai/browser-policy.yaml.
type browserPolicy struct {
	SensitiveDomains []string `yaml:"sensitive_domains"`
	BlockedPatterns  []string `yaml:"blocked_patterns"`
	TrustedDomains   []string `yaml:"trusted_domains"`
	StrictMode       bool     `yaml:"strict_mode"`
}

// auditEntry is one JSONL line in ~/.qai/browser-audit.log.
type auditEntry struct {
	Timestamp string `json:"ts"`
	Command   string `json:"cmd"`
	Domain    string `json:"domain"`
	Args      string `json:"args"`
	Allowed   bool   `json:"allowed"`
	Reason    string `json:"reason"`
	TabID     string `json:"tab_id"`
}

// ─── Layer 1: deny-list patterns ──────────────────────────────────────────

// builtinDenyPatterns are hard-blocked in eval expressions. No user override.
var builtinDenyPatterns = []string{
	`document\.cookie`,
	`localStorage`,
	`sessionStorage`,
	`indexedDB`,
	`fetch\s*\(`,
	`XMLHttpRequest`,
	`navigator\.sendBeacon`,
	`window\.open\s*\(`,
	`\beval\s*\(`,
	`\bFunction\s*\(`,
	`chrome\.runtime`,
	`ServiceWorker`,
	`importScripts`,
}

var compiledDenyPatterns []*regexp.Regexp

// ─── Layer 2: sensitive domains ───────────────────────────────────────────

var builtinSensitiveDomains = []string{
	// Cloud consoles
	"*.aws.amazon.com",
	"*.console.aws.amazon.com",
	"console.cloud.google.com",
	"portal.azure.com",
	"dash.cloudflare.com",
	// Code & CI
	"github.com",
	"*.github.com",
	"*.gitlab.com",
	"*.bitbucket.org",
	"*.jenkins.io",
	"*.circleci.com",
	// Auth & SSO
	"accounts.google.com",
	"*.okta.com",
	"*.auth0.com",
	"login.microsoftonline.com",
	// Password managers
	"*.1password.com",
	"*.bitwarden.com",
	"*.lastpass.com",
	// Banking
	"*.chase.com",
	"*.wellsfargo.com",
	"*.bankofamerica.com",
	"*.citi.com",
	"*.capitalone.com",
	// Payments
	"*.stripe.com",
	"*.paypal.com",
	"*.braintreepayments.com",
	// Crypto
	"*.coinbase.com",
	"*.binance.com",
	"*.kraken.com",
	// Email
	"mail.google.com",
	"outlook.live.com",
	"outlook.office365.com",
}

var loadedPolicy browserPolicy
var compiledSensitive []string
var compiledTrusted []string

// ─── init ─────────────────────────────────────────────────────────────────

func init() {
	// Load user policy (non-fatal if missing)
	policyPath := filepath.Join(config.Home, ".qai", "browser-policy.yaml")
	if data, err := os.ReadFile(policyPath); err == nil {
		yaml.Unmarshal(data, &loadedPolicy)
	}

	// Compile deny-list: built-in + user additions
	allPatterns := append(builtinDenyPatterns, loadedPolicy.BlockedPatterns...)
	for _, p := range allPatterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai browser: bad deny pattern %q: %v\n", p, err)
			continue
		}
		compiledDenyPatterns = append(compiledDenyPatterns, re)
	}

	// Merge sensitive domains
	compiledSensitive = append(builtinSensitiveDomains, loadedPolicy.SensitiveDomains...)
	compiledTrusted = loadedPolicy.TrustedDomains
}

// ─── Layer 1: pattern block ───────────────────────────────────────────────

func checkEvalSafety(expr string) error {
	for _, re := range compiledDenyPatterns {
		if re.MatchString(expr) {
			return fmt.Errorf("blocked: expression matches dangerous pattern %q", re.String())
		}
	}
	return nil
}

// ─── Layer 2: domain sensitivity ──────────────────────────────────────────

func checkDomainSensitivity(rawURL string) bool {
	host := extractHost(rawURL)
	if host == "" {
		return false
	}

	// Trusted domains bypass
	for _, pattern := range compiledTrusted {
		if domainMatches(host, pattern) {
			return false
		}
	}

	// Strict mode: everything not trusted is sensitive
	if loadedPolicy.StrictMode {
		return true
	}

	// Check sensitive list
	for _, pattern := range compiledSensitive {
		if domainMatches(host, pattern) {
			return true
		}
	}
	return false
}

func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func domainMatches(host, pattern string) bool {
	pattern = strings.ToLower(pattern)
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".github.com"
		return host == pattern[2:] || strings.HasSuffix(host, suffix)
	}
	return host == pattern
}

// ─── Layer 3: TTY confirmation ────────────────────────────────────────────

func confirmAction(action, domain string) bool {
	// Check for --yes flag (trusted automation bypass)
	for _, a := range os.Args {
		if a == "--yes" || a == "-y" {
			return true
		}
	}

	// Must be a TTY
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintf(os.Stderr, "qai browser: denied %s on %s (non-interactive, use --yes to override)\n", action, domain)
		return false
	}

	fmt.Fprintf(os.Stderr, "\n⚠ Execute %s on %s? [y/N]: ", action, domain)
	var response string
	fmt.Scanln(&response)
	return strings.ToLower(strings.TrimSpace(response)) == "y"
}

// ─── Layer 4: audit log ──────────────────────────────────────────────────

func auditLog(entry auditEntry) {
	entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	logPath := filepath.Join(config.Home, ".qai", "browser-audit.log")
	os.MkdirAll(filepath.Dir(logPath), 0755)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return // non-fatal
	}
	defer f.Close()

	line, _ := json.Marshal(entry)
	f.Write(append(line, '\n'))
}

// ─── orchestrator ─────────────────────────────────────────────────────────

// securityGate runs all four layers. Called from every browser command.
func securityGate(action string, tab *cdpTab, detail string) error {
	domain := extractHost(tab.URL)
	entry := auditEntry{
		Command: action,
		Domain:  domain,
		Args:    strutil.TruncateStr(detail, 500),
		TabID:   tab.ID,
	}

	// Layer 1: Pattern block (eval only)
	if action == "eval" {
		if err := checkEvalSafety(detail); err != nil {
			entry.Allowed = false
			entry.Reason = "pattern_blocked"
			auditLog(entry)
			fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
			return err
		}
	}

	// Layer 2+3: Domain sensitivity → TTY confirmation
	if checkDomainSensitivity(tab.URL) {
		if !confirmAction(action, domain) {
			entry.Allowed = false
			entry.Reason = "user_denied"
			auditLog(entry)
			fmt.Fprintf(os.Stderr, "qai browser: denied %s on sensitive domain %s\n", action, domain)
			return fmt.Errorf("denied: %s on sensitive domain %s", action, domain)
		}
		entry.Reason = "user_approved"
		for _, a := range os.Args {
			if a == "--yes" || a == "-y" {
				entry.Reason = "auto_approved_yes_flag"
			}
		}
	} else {
		entry.Reason = "domain_allowed"
	}

	// Layer 4: Audit log (always, regardless of outcome)
	entry.Allowed = true
	auditLog(entry)
	return nil
}

