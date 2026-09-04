package auth

import (
	"testing"
)

func TestWriteUserThenReadUserRoundTrips(t *testing.T) {
	dir := t.TempDir()
	u := &User{
		Login:     "sjawhar",
		Tokens:    Tokens{AccessToken: "ghu_abc", GithubLogin: "sjawhar"},
		Addressed: map[string]string{"sjawhar/legion#42": "2026-09-04T00:00:00Z"},
	}
	if err := WriteUser(dir, u); err != nil {
		t.Fatalf("WriteUser: %v", err)
	}
	got, err := ReadUser(dir, "sjawhar")
	if err != nil {
		t.Fatalf("ReadUser: %v", err)
	}
	if got == nil {
		t.Fatalf("expected non-nil user")
	}
	if got.Login != "sjawhar" || got.Tokens.AccessToken != "ghu_abc" {
		t.Errorf("scalar mismatch: %+v", got)
	}
	if got.Addressed["sjawhar/legion#42"] != "2026-09-04T00:00:00Z" {
		t.Errorf("addressed mismatch: %+v", got.Addressed)
	}
}

func TestReadUserMissingFileReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadUser(dir, "nobody")
	if err != nil {
		t.Fatalf("ReadUser: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil user for missing file, got %+v", got)
	}
}

func TestUserPathRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	if _, err := userPath(dir, "../../etc/passwd"); err == nil {
		t.Errorf("expected an error for a path-traversal login")
	}
}

func TestRemoveUserThenReadUserReturnsNil(t *testing.T) {
	dir := t.TempDir()
	u := &User{Login: "sjawhar", Tokens: Tokens{GithubLogin: "sjawhar"}}
	if err := WriteUser(dir, u); err != nil {
		t.Fatalf("WriteUser: %v", err)
	}
	if err := RemoveUser(dir, "sjawhar"); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	got, err := ReadUser(dir, "sjawhar")
	if err != nil {
		t.Fatalf("ReadUser: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil user after removal, got %+v", got)
	}
}
