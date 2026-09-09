package auth

// User is the persisted OAuth record for one authenticated dashboard user.
type User struct {
	Login  string `json:"login"`
	Tokens Tokens `json:"tokens"`
}

// UserStore persists refreshable GitHub OAuth token pairs. Read returns
// nil, nil when no record exists for login.
type UserStore interface {
	Read(login string) (*User, error)
	Write(user *User) error
	Remove(login string) error
}
