package core

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

func now() time.Time { return time.Now().UTC() }

var slug = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var owner = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
var file = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.ya?ml$`)
var secretPattern = regexp.MustCompile(`(?i)(bearer\s+\S+|(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]+|(?:api[_-]?key|token|password|secret|authorization)\s*[:=]\s*[^\s,;]+)`)

func Redact(s string) string { return secretPattern.ReplaceAllString(s, "[REDACTED]") }
func ValidateDeployment(a Deployment) error {
	if strings.TrimSpace(a.Name) == "" || len(a.Name) > 120 {
		return fmt.Errorf("name must contain 1–120 characters")
	}
	if len(a.Description) > 2000 {
		return fmt.Errorf("description exceeds 2000 characters")
	}
	if !owner.MatchString(a.Owner) || !slug.MatchString(a.Repository) || len(a.Owner) > 39 || len(a.Repository) > 100 || a.Repository == "." || a.Repository == ".." {
		return fmt.Errorf("provide a valid GitHub owner and repository")
	}
	if !file.MatchString(a.Workflow) {
		return fmt.Errorf("provide a workflow filename ending in .yml or .yaml")
	}
	if !validRef(a.Ref) {
		return fmt.Errorf("provide a valid Git branch or tag reference")
	}
	if a.InstallationID <= 0 {
		return fmt.Errorf("GitHub App installation ID must be positive")
	}
	if a.BudgetMicros < 0 || a.BudgetMicros > 1_000_000_000_000 {
		return fmt.Errorf("budget must be between $0 and $1,000,000")
	}
	return nil
}
func validRef(ref string) bool {
	if ref == "" || len(ref) > 250 || strings.ContainsAny(ref, " ~^:?*[\\\t\r\n") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
		return false
	}
	return true
}
func refMatches(configured, actual string) bool {
	if actual == configured {
		return true
	}
	if strings.HasPrefix(configured, "refs/") {
		return !strings.HasPrefix(actual, "refs/") && (configured == "refs/heads/"+actual || configured == "refs/tags/"+actual)
	}
	return actual == "refs/heads/"+configured
}
func Terminal(s string) bool { return s == "completed" || s == "failed" || s == "cancelled" }

// Terminal states never regress.
func Transition(current, next string) string {
	if Terminal(current) {
		return current
	}
	if next == "running" && (current == "queued" || current == "running") {
		return "running"
	}
	if Terminal(next) {
		return next
	}
	return current
}
