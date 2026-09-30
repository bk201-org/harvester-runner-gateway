package config

import (
	"strings"
	"testing"
)

func TestDeveloperConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, id, repo, path string
		duplicate, bad       bool
	}{
		{name: "valid", id: "alice-1", repo: "123", path: "/private/token"},
		{name: "empty", repo: "123", path: "/private/token", bad: true},
		{name: "uppercase", id: "Alice", repo: "123", path: "/private/token", bad: true},
		{name: "separator", id: "alice,bob", repo: "123", path: "/private/token", bad: true},
		{name: "long", id: strings.Repeat("a", 60), repo: "123", path: "/private/token", bad: true},
		{name: "boundary", id: strings.Repeat("a", 59), repo: "123", path: "/private/token"},
		{name: "unknown policy", id: "alice", repo: "456", path: "/private/token", bad: true},
		{name: "relative path", id: "alice", repo: "123", path: "token", bad: true},
		{name: "duplicate", id: "alice", repo: "123", path: "/private/token", duplicate: true, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Repositories: []RepositoryPolicy{{RepositoryID: "123"}}, Developers: []DeveloperConfig{{ID: tc.id, RepositoryID: tc.repo, TokenFile: tc.path}}}
			if tc.duplicate {
				c.Developers = append(c.Developers, c.Developers[0])
			}
			if err := c.ValidateDevelopers(); (err != nil) != tc.bad {
				t.Fatalf("error=%v want invalid=%v", err, tc.bad)
			}
		})
	}
}
