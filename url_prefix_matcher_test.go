package loggingproxy

import (
	"net/url"
	"testing"
)

func TestURLPrefixMatcherMatchesDefaultPortAndPathPrefix(t *testing.T) {
	matcher, err := newURLPrefixMatcher([]string{"https://openrouter.ai/api/v1/models/"})
	if err != nil {
		t.Fatalf("newURLPrefixMatcher failed: %v", err)
	}

	target, err := url.Parse("https://openrouter.ai:443/api/v1/models/list?limit=1000")
	if err != nil {
		t.Fatalf("failed to parse target URL: %v", err)
	}
	if !matcher.Match(target) {
		t.Fatal("expected URL prefix matcher to match default-port target")
	}
}

func TestURLPrefixMatcherWithoutTrailingSlashMatchesOnlyExactEndpoint(t *testing.T) {
	matcher, err := newURLPrefixMatcher([]string{"https://openrouter.ai/api/v1/models"})
	if err != nil {
		t.Fatalf("newURLPrefixMatcher failed: %v", err)
	}

	matchingTarget, err := url.Parse("https://openrouter.ai/api/v1/models?limit=1000")
	if err != nil {
		t.Fatalf("failed to parse matching target URL: %v", err)
	}
	if !matcher.Match(matchingTarget) {
		t.Fatal("expected URL prefix matcher to match exact endpoint")
	}

	nonMatchingTarget, err := url.Parse("https://openrouter.ai/api/v1/models/list")
	if err != nil {
		t.Fatalf("failed to parse non-matching target URL: %v", err)
	}
	if matcher.Match(nonMatchingTarget) {
		t.Fatal("expected URL prefix matcher not to match subpath without trailing slash")
	}
}

func TestURLPrefixMatcherDoesNotMatchDifferentPath(t *testing.T) {
	matcher, err := newURLPrefixMatcher([]string{"https://openrouter.ai/api/v1/models/"})
	if err != nil {
		t.Fatalf("newURLPrefixMatcher failed: %v", err)
	}

	target, err := url.Parse("https://openrouter.ai/api/v1/chat/completions")
	if err != nil {
		t.Fatalf("failed to parse target URL: %v", err)
	}
	if matcher.Match(target) {
		t.Fatal("expected URL prefix matcher not to match different path")
	}
}

func TestURLPrefixMatcherRejectsRelativePrefix(t *testing.T) {
	_, err := newURLPrefixMatcher([]string{"/api/v1/models/"})
	if err == nil {
		t.Fatal("expected relative prefix to be rejected")
	}
}
