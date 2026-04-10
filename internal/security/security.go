// security.go — AI vulnerability scanner.
//
// Shells out to rust-security-detector for multi-language security analysis.
// Detects: SQL injection, command injection, XSS, hardcoded secrets,
// path traversal, insecure deserialization, weak crypto, prompt injection,
// RAG poisoning, and 30+ more vulnerability types with CWE mappings.

package security

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type SecurityResult struct {
	Vulnerabilities []VulnFinding `json:"vulnerabilities"`
	FilesScanned    int           `json:"files_scanned"`
	LinesScanned    int           `json:"lines_scanned"`
}

type VulnFinding struct {
	FilePath    string `json:"file_path"`
	LineNumber  int    `json:"line_number"`
	VulnType    string `json:"vulnerability_type"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Remediation string `json:"remediation,omitempty"`
	CodeSnippet string `json:"code_snippet,omitempty"`
	CWEID       string `json:"cwe_id,omitempty"`
	OWASPCat    string `json:"owasp_category,omitempty"`
	Confidence  string `json:"confidence"`
}

func CmdSecurity(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `usage: qai security <path> [options]

Scan code for security vulnerabilities with remediation guidance.
Uses rust-security-detector (14 languages, 40+ vuln types, CWE mapped).

Options:
  --format <fmt>       Output: summary (default), json, table
  --threat-model       Include PASTA threat model with MITRE mapping
  --cve                Check against CVE database
  --performance        Scan for performance issues only
  --all                Security + performance scan
  -o <file>            Output to file`)
		os.Exit(1)
	}

	dir := args[0]
	format := "summary"
	threatModel := false
	cveCheck := false
	perfOnly := false
	scanAll := false
	output := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--format", "-f":
			if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		case "--threat-model":
			threatModel = true
		case "--cve":
			cveCheck = true
		case "--performance":
			perfOnly = true
		case "--all":
			scanAll = true
		case "-o", "--output":
			if i+1 < len(args) {
				output = args[i+1]
				i++
			}
		}
	}

	absDir, _ := filepath.Abs(dir)

	// Find rust-security-detector.
	scanner, err := exec.LookPath("rust-security-detector")
	if err != nil {
		fmt.Fprintln(os.Stderr, "rust-security-detector not found on PATH")
		fmt.Fprintln(os.Stderr, "Install: cargo install rust-security-detector")
		fmt.Fprintln(os.Stderr, "Or build from source: https://github.com/quantum-encoding/rust-security")
		os.Exit(1)
	}

	// Build command.
	cmdArgs := []string{"scan", absDir, "--format", "json"}
	if threatModel {
		cmdArgs = append(cmdArgs, "--threat-model")
	}
	if cveCheck {
		cmdArgs = append(cmdArgs, "--enable-cve")
	}
	if perfOnly {
		cmdArgs = append(cmdArgs, "--performance")
	} else if scanAll {
		cmdArgs = append(cmdArgs, "--all")
	}

	fmt.Fprintf(os.Stderr, "Scanning %s for vulnerabilities...\n", absDir)

	cmd := exec.Command(scanner, cmdArgs...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		// Scanner may exit non-zero if vulnerabilities found — that's expected.
		if exitErr, ok := err.(*exec.ExitError); ok && len(out) > 0 {
			_ = exitErr // has findings
		} else {
			fmt.Fprintf(os.Stderr, "security scanner failed: %v\n", err)
			os.Exit(1)
		}
	}

	// Parse results.
	var result SecurityResult
	if err := json.Unmarshal(out, &result); err != nil {
		// Try parsing as array of findings directly.
		var findings []VulnFinding
		if err2 := json.Unmarshal(out, &findings); err2 != nil {
			// Output raw if can't parse.
			if format == "json" {
				fmt.Print(string(out))
			} else {
				fmt.Fprintf(os.Stderr, "Could not parse scanner output, showing raw:\n")
				fmt.Print(string(out))
			}
			return
		}
		result.Vulnerabilities = findings
	}

	// Format output.
	var formatted string
	switch format {
	case "json":
		data, _ := json.MarshalIndent(result, "", "  ")
		formatted = string(data)
	case "table":
		formatted = formatSecurityTable(result, absDir)
	default:
		formatted = formatSecuritySummary(result, absDir)
	}

	if output != "" {
		if err := os.WriteFile(output, []byte(formatted), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "qai security: write %s: %v\n", output, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Wrote %s\n", output)
	} else {
		fmt.Print(formatted)
	}
}

func formatSecuritySummary(result SecurityResult, dir string) string {
	var b strings.Builder

	critical, high, medium, low := countBySeverity(result.Vulnerabilities)
	total := len(result.Vulnerabilities)

	b.WriteString(fmt.Sprintf("Security Scan: %s\n", filepath.Base(dir)))
	b.WriteString(fmt.Sprintf("  Findings: %d | Critical: %d, High: %d, Medium: %d, Low: %d\n\n",
		total, critical, high, medium, low))

	if total == 0 {
		b.WriteString("  No vulnerabilities found.\n")
		return b.String()
	}

	// Group by severity.
	for _, sev := range []string{"Critical", "High", "Medium", "Low"} {
		findings := filterBySeverity(result.Vulnerabilities, sev)
		if len(findings) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("  %s (%d):\n", strings.ToUpper(sev), len(findings)))
		for _, f := range findings {
			file := filepath.Base(f.FilePath)
			b.WriteString(fmt.Sprintf("    %s:%d  %s\n", file, f.LineNumber, f.Description))
			if f.CWEID != "" {
				b.WriteString(fmt.Sprintf("      %s", f.CWEID))
			}
			if f.Remediation != "" {
				b.WriteString(fmt.Sprintf(" | Fix: %s", f.Remediation))
			}
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	return b.String()
}

func formatSecurityTable(result SecurityResult, dir string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%-10s %-30s %-6s %-40s %s\n", "Severity", "File:Line", "CWE", "Vulnerability", "Fix"))
	b.WriteString(strings.Repeat("-", 120) + "\n")

	for _, f := range result.Vulnerabilities {
		file := filepath.Base(f.FilePath)
		loc := fmt.Sprintf("%s:%d", file, f.LineNumber)
		desc := f.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		fix := f.Remediation
		if len(fix) > 30 {
			fix = fix[:27] + "..."
		}
		b.WriteString(fmt.Sprintf("%-10s %-30s %-6s %-40s %s\n", f.Severity, loc, f.CWEID, desc, fix))
	}
	return b.String()
}

func countBySeverity(findings []VulnFinding) (critical, high, medium, low int) {
	for _, f := range findings {
		switch strings.ToLower(f.Severity) {
		case "critical":
			critical++
		case "high":
			high++
		case "medium":
			medium++
		case "low":
			low++
		}
	}
	return
}

func filterBySeverity(findings []VulnFinding, severity string) []VulnFinding {
	var filtered []VulnFinding
	for _, f := range findings {
		if strings.EqualFold(f.Severity, severity) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}
