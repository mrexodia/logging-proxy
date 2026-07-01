package loggingproxy

import "testing"

func TestMITMExcludeMatcher(t *testing.T) {
	matcher, err := newMITMExcludeMatcher([]string{
		"api.example.com",
		"*.internal.example",
		"127.0.0.1",
		"10.0.0.0/8",
		"2001:db8::/32",
	})
	if err != nil {
		t.Fatalf("failed to build matcher: %v", err)
	}

	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "exact host", host: "api.example.com:443", want: true},
		{name: "case insensitive", host: "API.EXAMPLE.COM:443", want: true},
		{name: "wildcard subdomain", host: "chat.internal.example:443", want: true},
		{name: "wildcard root excluded only by exact rule", host: "internal.example:443", want: false},
		{name: "ip literal", host: "127.0.0.1:443", want: true},
		{name: "cidr ipv4", host: "10.2.3.4:443", want: true},
		{name: "cidr ipv6", host: "[2001:db8::1]:443", want: true},
		{name: "not matched", host: "api.example.org:443", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matcher.Match(tt.host); got != tt.want {
				t.Fatalf("Match(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestMITMExcludeMatcherMatchAll(t *testing.T) {
	matcher, err := newMITMExcludeMatcher([]string{"*"})
	if err != nil {
		t.Fatalf("failed to build matcher: %v", err)
	}
	if !matcher.Match("anything.example:443") {
		t.Fatal("expected wildcard matcher to match all hosts")
	}
}

func TestMITMExcludeMatcherRejectsInvalidCIDR(t *testing.T) {
	_, err := newMITMExcludeMatcher([]string{"10.0.0.0/not-a-prefix"})
	if err == nil {
		t.Fatal("expected invalid CIDR to fail")
	}
}
